use crate::SPEC_ENVIRONMENT;
use crate::ready::ReadyState;
use crate::spec::{
    Bind, MainProcess, NumericUser, Probe, ProcessKind, ProcessUser, SidecarProcess, Spec,
};
use std::collections::{BTreeMap, HashSet};
use std::error::Error;
use std::fmt;
use std::fs::{self, File};
use std::io::{self, Read, Write};
use std::net::{IpAddr, Ipv4Addr, SocketAddr, TcpStream};
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::ptr;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError, Sender};
use std::thread;
use std::time::{Duration, Instant};

const SHUTDOWN_GRACE: Duration = Duration::from_secs(5);
const KILL_GRACE: Duration = Duration::from_secs(1);
const POLL_INTERVAL: Duration = Duration::from_millis(50);
const MAX_PASSWD_BYTES: u64 = 1024 * 1024;
const LINUX_CAPABILITY_VERSION_3: u32 = 0x2008_0522;
static RECEIVED_SIGNAL: AtomicI32 = AtomicI32::new(0);

#[repr(C)]
struct CapabilityHeader {
    version: u32,
    pid: i32,
}

#[repr(C)]
#[derive(Clone, Copy)]
struct CapabilityData {
    effective: u32,
    permitted: u32,
    inheritable: u32,
}

#[derive(Debug)]
pub struct RuntimeError(String);

impl RuntimeError {
    fn new(message: impl Into<String>) -> Self {
        Self(message.into())
    }
}

impl fmt::Display for RuntimeError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl Error for RuntimeError {}

#[derive(Clone, Debug)]
enum ProcessSource {
    Main {
        user: Option<NumericUser>,
    },
    Sidecar {
        rootfs: String,
        overlay_device: Option<String>,
        standard_mounts: bool,
        disk_mounts: Vec<String>,
        binds: Vec<Bind>,
        user: Option<ProcessUser>,
    },
}

#[derive(Clone, Debug)]
struct LaunchProcess {
    name: String,
    kind: ProcessKind,
    command: Vec<String>,
    env: BTreeMap<String, String>,
    workdir: String,
    readiness_probe: Option<Probe>,
    completion_timeout_ms: Option<u64>,
    source: ProcessSource,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum ProbeHealth {
    Starting,
    Ready,
    Unready,
}

#[derive(Debug)]
struct ProbeState {
    health: ProbeHealth,
    consecutive_successes: u32,
    consecutive_failures: u32,
    success_threshold: u32,
    failure_threshold: u32,
}

impl ProbeState {
    fn new(probe: &Probe) -> Self {
        Self {
            health: ProbeHealth::Starting,
            consecutive_successes: 0,
            consecutive_failures: 0,
            success_threshold: probe.success_threshold,
            failure_threshold: probe.failure_threshold,
        }
    }

    fn observe(&mut self, success: bool) -> Option<(ProbeHealth, ProbeHealth)> {
        let previous = self.health;
        if success {
            self.consecutive_failures = 0;
            self.consecutive_successes = self.consecutive_successes.saturating_add(1);
            if self.consecutive_successes >= self.success_threshold {
                self.health = ProbeHealth::Ready;
            }
        } else {
            self.consecutive_successes = 0;
            self.consecutive_failures = self.consecutive_failures.saturating_add(1);
            if self.consecutive_failures >= self.failure_threshold {
                self.health = ProbeHealth::Unready;
            }
        }
        (previous != self.health).then_some((previous, self.health))
    }
}

#[derive(Debug)]
struct ManagedProcess {
    name: String,
    kind: ProcessKind,
    pid: libc::pid_t,
    alive: bool,
    exit_status: Option<libc::c_int>,
    residual_group: bool,
    probe: Option<ProbeState>,
    probe_cancel: Option<Arc<AtomicBool>>,
}

#[derive(Debug)]
struct ProbeEvent {
    process_index: usize,
    success: bool,
}

#[derive(Debug)]
struct PendingCleanup {
    group: libc::pid_t,
    kill_at: Instant,
    give_up_at: Instant,
    killed: bool,
}

struct Supervisor {
    ready: ReadyState,
    processes: Vec<ManagedProcess>,
    probe_sender: Sender<ProbeEvent>,
    probe_receiver: Receiver<ProbeEvent>,
    cleanups: Vec<PendingCleanup>,
    initialization_complete: bool,
    startup_failure: Option<String>,
}

enum WaitOutcome {
    Completed,
    Failed(String),
    Signal(i32),
}

pub fn run(spec: Spec, ready: ReadyState) -> Result<i32, RuntimeError> {
    RECEIVED_SIGNAL.store(0, Ordering::Release);
    install_runtime_contract()?;
    let sandrun = sandrun_path()?;
    let declarations = flatten(spec)?;
    let mut supervisor = Supervisor::new(ready);

    for process in declarations {
        if let Some(signal) = supervisor.drive(Duration::ZERO)? {
            return Ok(supervisor.shutdown(signal));
        }
        if let Some(reason) = supervisor.startup_failure.clone() {
            supervisor.fail_startup(reason);
            break;
        }

        let name = process.name.clone();
        let kind = process.kind;
        let startup_timeout = process
            .readiness_probe
            .as_ref()
            .map(|probe| probe.startup_timeout_ms);
        let completion_timeout = process.completion_timeout_ms;
        let process_index = match supervisor.spawn(process, &sandrun) {
            Ok(index) => index,
            Err(error) => {
                supervisor.fail_startup(format!("failed to start {name}: {error}"));
                break;
            }
        };

        let outcome = match kind {
            ProcessKind::Service => {
                if let Some(timeout) = startup_timeout {
                    supervisor.wait_for_service(process_index, Duration::from_millis(timeout))?
                } else {
                    WaitOutcome::Completed
                }
            }
            ProcessKind::RunToCompletion => supervisor.wait_for_job(
                process_index,
                Duration::from_millis(completion_timeout.expect("validated completion timeout")),
            )?,
        };
        match outcome {
            WaitOutcome::Completed => {}
            WaitOutcome::Failed(reason) => {
                supervisor.fail_startup(reason);
                break;
            }
            WaitOutcome::Signal(signal) => return Ok(supervisor.shutdown(signal)),
        }
    }

    if supervisor.startup_failure.is_none()
        && let Some(signal) = supervisor.drive(Duration::ZERO)?
    {
        return Ok(supervisor.shutdown(signal));
    }
    if supervisor.startup_failure.is_none() {
        supervisor.initialization_complete = true;
        eprintln!("campd: initialization completed");
    }
    supervisor.update_ready();
    supervisor.supervise()
}

impl Supervisor {
    fn new(ready: ReadyState) -> Self {
        let (probe_sender, probe_receiver) = mpsc::channel();
        Self {
            ready,
            processes: Vec::new(),
            probe_sender,
            probe_receiver,
            cleanups: Vec::new(),
            initialization_complete: false,
            startup_failure: None,
        }
    }

    fn spawn(&mut self, process: LaunchProcess, sandrun: &Path) -> io::Result<usize> {
        let mut command = build_command(&process, sandrun);
        let numeric_user = match &process.source {
            ProcessSource::Main { user } => *user,
            ProcessSource::Sidecar { .. } => None,
        };
        unsafe {
            command.pre_exec(move || {
                if libc::setpgid(0, 0) != 0 {
                    return Err(io::Error::last_os_error());
                }
                if let Some(user) = numeric_user {
                    apply_main_identity(user)?;
                }
                Ok(())
            });
        }
        let child = command.spawn()?;
        let pid = child.id() as libc::pid_t;
        drop(child);

        let process_index = self.processes.len();
        let probe = process.readiness_probe.as_ref().map(ProbeState::new);
        let probe_cancel = if let Some(configured) = process.readiness_probe.as_ref() {
            let cancel = Arc::new(AtomicBool::new(false));
            if let Err(error) = spawn_probe_worker(
                process_index,
                process.name.clone(),
                configured.clone(),
                Arc::clone(&cancel),
                self.probe_sender.clone(),
            ) {
                signal_group(pid, libc::SIGKILL);
                reap_pid(pid);
                return Err(error);
            }
            Some(cancel)
        } else {
            None
        };
        self.processes.push(ManagedProcess {
            name: process.name,
            kind: process.kind,
            pid,
            alive: true,
            exit_status: None,
            residual_group: false,
            probe,
            probe_cancel,
        });
        eprintln!(
            "campd: started process={} kind={:?} pid={pid}",
            self.processes[process_index].name, self.processes[process_index].kind
        );
        self.update_ready();
        Ok(process_index)
    }

    fn wait_for_service(
        &mut self,
        process_index: usize,
        timeout: Duration,
    ) -> Result<WaitOutcome, RuntimeError> {
        let deadline = Instant::now() + timeout;
        loop {
            if let Some(signal) = self.drive(wait_duration(deadline))? {
                return Ok(WaitOutcome::Signal(signal));
            }
            if let Some(reason) = self.startup_failure.clone() {
                return Ok(WaitOutcome::Failed(reason));
            }
            let process = &self.processes[process_index];
            if process
                .probe
                .as_ref()
                .is_some_and(|probe| probe.health == ProbeHealth::Ready)
            {
                eprintln!("campd: readiness probe passed process={}", process.name);
                return Ok(WaitOutcome::Completed);
            }
            if Instant::now() >= deadline {
                return Ok(WaitOutcome::Failed(format!(
                    "readiness probe timed out for {} after {}ms",
                    process.name,
                    timeout.as_millis()
                )));
            }
        }
    }

    fn wait_for_job(
        &mut self,
        process_index: usize,
        timeout: Duration,
    ) -> Result<WaitOutcome, RuntimeError> {
        let deadline = Instant::now() + timeout;
        loop {
            if let Some(signal) = self.drive(wait_duration(deadline))? {
                return Ok(WaitOutcome::Signal(signal));
            }
            if let Some(reason) = self.startup_failure.clone() {
                let process = &self.processes[process_index];
                if process.alive {
                    self.schedule_cleanup(process.pid);
                }
                return Ok(WaitOutcome::Failed(reason));
            }
            let process = &self.processes[process_index];
            if let Some(status) = process.exit_status {
                if exit_code(status) != 0 {
                    return Ok(WaitOutcome::Failed(format!(
                        "run-to-completion process {} failed with {}",
                        process.name,
                        display_status(status)
                    )));
                }
                if process.residual_group {
                    return Ok(WaitOutcome::Failed(format!(
                        "run-to-completion process {} left descendants in its process group",
                        process.name
                    )));
                }
                eprintln!(
                    "campd: run-to-completion process succeeded process={}",
                    process.name
                );
                return Ok(WaitOutcome::Completed);
            }
            if Instant::now() >= deadline {
                let name = process.name.clone();
                let group = process.pid;
                self.schedule_cleanup(group);
                return Ok(WaitOutcome::Failed(format!(
                    "run-to-completion process {name} timed out after {}ms",
                    timeout.as_millis()
                )));
            }
        }
    }

    fn drive(&mut self, wait: Duration) -> Result<Option<i32>, RuntimeError> {
        self.reap_available()?;
        self.drain_probe_events();
        self.process_cleanups();
        self.update_ready();
        if let Some(signal) = received_signal() {
            return Ok(Some(signal));
        }

        let event = if wait.is_zero() {
            self.probe_receiver.try_recv().ok()
        } else {
            match self.probe_receiver.recv_timeout(wait.min(POLL_INTERVAL)) {
                Ok(event) => Some(event),
                Err(RecvTimeoutError::Timeout) => None,
                Err(RecvTimeoutError::Disconnected) => None,
            }
        };
        if let Some(event) = event {
            self.apply_probe_event(event);
            self.drain_probe_events();
        }
        self.reap_available()?;
        self.process_cleanups();
        self.update_ready();
        Ok(received_signal())
    }

    fn reap_available(&mut self) -> Result<(), RuntimeError> {
        loop {
            let mut status = 0;
            let pid = unsafe { libc::waitpid(-1, &mut status, libc::WNOHANG) };
            if pid > 0 {
                let Some(index) = self
                    .processes
                    .iter()
                    .position(|process| process.pid == pid && process.alive)
                else {
                    continue;
                };
                let residual_group = process_group_exists(pid);
                let process = &mut self.processes[index];
                process.alive = false;
                process.exit_status = Some(status);
                process.residual_group = residual_group;
                if let Some(cancel) = &process.probe_cancel {
                    cancel.store(true, Ordering::Release);
                }
                eprintln!(
                    "campd: process exited process={} pid={pid} status={}",
                    process.name,
                    display_status(status)
                );
                if process.kind == ProcessKind::Service && !self.initialization_complete {
                    self.startup_failure.get_or_insert_with(|| {
                        format!(
                            "service process {} exited during initialization with {}",
                            process.name,
                            display_status(status)
                        )
                    });
                }
                if residual_group {
                    self.schedule_cleanup(pid);
                }
                continue;
            }
            if pid == 0 {
                return Ok(());
            }
            let error = io::Error::last_os_error();
            if error.kind() == io::ErrorKind::Interrupted {
                continue;
            }
            if error.raw_os_error() == Some(libc::ECHILD) {
                return Ok(());
            }
            return Err(RuntimeError::new(format!("wait for processes: {error}")));
        }
    }

    fn drain_probe_events(&mut self) {
        while let Ok(event) = self.probe_receiver.try_recv() {
            self.apply_probe_event(event);
        }
    }

    fn apply_probe_event(&mut self, event: ProbeEvent) {
        let Some(process) = self.processes.get_mut(event.process_index) else {
            return;
        };
        if !process.alive {
            return;
        }
        let Some(probe) = process.probe.as_mut() else {
            return;
        };
        if let Some((previous, current)) = probe.observe(event.success) {
            eprintln!(
                "campd: readiness changed process={} from={previous:?} to={current:?}",
                process.name
            );
        }
    }

    fn update_ready(&self) {
        let services_ready = self
            .processes
            .iter()
            .filter(|process| process.kind == ProcessKind::Service)
            .all(|process| {
                process.alive
                    && process
                        .probe
                        .as_ref()
                        .is_none_or(|probe| probe.health == ProbeHealth::Ready)
            });
        self.ready
            .set(self.initialization_complete && self.startup_failure.is_none() && services_ready);
    }

    fn fail_startup(&mut self, reason: String) {
        if self.startup_failure.is_none() {
            self.startup_failure = Some(reason.clone());
        }
        eprintln!("campd: initialization failed: {reason}");
        self.initialization_complete = false;
        self.update_ready();
    }

    fn schedule_cleanup(&mut self, group: libc::pid_t) {
        if !process_group_exists(group)
            || self.cleanups.iter().any(|cleanup| cleanup.group == group)
        {
            return;
        }
        signal_group(group, libc::SIGTERM);
        let now = Instant::now();
        self.cleanups.push(PendingCleanup {
            group,
            kill_at: now + SHUTDOWN_GRACE,
            give_up_at: now + SHUTDOWN_GRACE + KILL_GRACE,
            killed: false,
        });
    }

    fn process_cleanups(&mut self) {
        let now = Instant::now();
        self.cleanups.retain_mut(|cleanup| {
            if !process_group_exists(cleanup.group) {
                return false;
            }
            if !cleanup.killed && now >= cleanup.kill_at {
                signal_group(cleanup.group, libc::SIGKILL);
                cleanup.killed = true;
            }
            if now >= cleanup.give_up_at {
                eprintln!(
                    "campd: process group survived cleanup group={}",
                    cleanup.group
                );
                return false;
            }
            true
        });
    }

    fn supervise(mut self) -> Result<i32, RuntimeError> {
        loop {
            if let Some(signal) = self.drive(POLL_INTERVAL)? {
                return Ok(self.shutdown(signal));
            }
        }
    }

    fn shutdown(&mut self, signal: i32) -> i32 {
        self.ready.set(false);
        for process in &self.processes {
            if let Some(cancel) = &process.probe_cancel {
                cancel.store(true, Ordering::Release);
            }
        }
        let groups = self
            .processes
            .iter()
            .filter(|process| process.alive || process.residual_group)
            .map(|process| process.pid)
            .chain(self.cleanups.iter().map(|cleanup| cleanup.group))
            .collect::<HashSet<_>>();
        terminate_groups(&groups, signal);
        eprintln!("campd: received signal={signal}");
        128 + signal
    }
}

// Main processes share campd's root filesystem. Resolve every named identity
// before the first declaration starts so an init job cannot change the
// identity of a later service by rewriting /etc/passwd.
fn resolve_main_users(
    processes: &[MainProcess],
) -> Result<BTreeMap<String, NumericUser>, RuntimeError> {
    if !processes
        .iter()
        .any(|process| matches!(&process.user, Some(ProcessUser::Named(_))))
    {
        return Ok(BTreeMap::new());
    }

    let contents = read_main_passwd(Path::new("/etc/passwd"))?;
    let mut resolved = BTreeMap::new();
    for process in processes {
        let Some(ProcessUser::Named(user)) = &process.user else {
            continue;
        };
        if !resolved.contains_key(&user.name) {
            resolved.insert(user.name.clone(), parse_main_passwd(&contents, &user.name)?);
        }
    }
    Ok(resolved)
}

fn read_main_passwd(path: &Path) -> Result<String, RuntimeError> {
    let metadata = fs::metadata(path).map_err(|error| {
        RuntimeError::new(format!(
            "main passwd_stat_failed: {}: {error}",
            path.display()
        ))
    })?;
    if !metadata.is_file() {
        return Err(RuntimeError::new(
            "main invalid_passwd_entry: /etc/passwd is not a regular file",
        ));
    }
    if metadata.len() > MAX_PASSWD_BYTES {
        return Err(RuntimeError::new("main passwd_too_large: /etc/passwd"));
    }
    let mut contents = Vec::with_capacity(usize::try_from(metadata.len()).unwrap_or(0));
    File::open(path)
        .and_then(|file| file.take(MAX_PASSWD_BYTES + 1).read_to_end(&mut contents))
        .map_err(|error| {
            RuntimeError::new(format!(
                "main passwd_read_failed: {}: {error}",
                path.display()
            ))
        })?;
    if contents.len() as u64 > MAX_PASSWD_BYTES {
        return Err(RuntimeError::new("main passwd_too_large: /etc/passwd"));
    }
    String::from_utf8(contents)
        .map_err(|_| RuntimeError::new("main invalid_passwd_entry: /etc/passwd is not UTF-8"))
}

fn parse_main_passwd(contents: &str, user: &str) -> Result<NumericUser, RuntimeError> {
    let mut found = None;
    for (index, line) in contents.lines().enumerate() {
        let line_number = index + 1;
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let fields = line.split(':').collect::<Vec<_>>();
        if fields.first().copied() != Some(user) {
            continue;
        }
        if found.is_some() {
            return Err(RuntimeError::new(format!(
                "main invalid_passwd_entry: line {line_number}: user name is duplicated"
            )));
        }
        if fields.len() != 7 {
            return Err(RuntimeError::new(format!(
                "main invalid_passwd_entry: line {line_number}: matching entry must contain seven fields"
            )));
        }
        let uid = parse_main_passwd_id(fields[2], line_number, "UID is invalid")?;
        let gid = parse_main_passwd_id(fields[3], line_number, "GID is invalid")?;
        found = Some(NumericUser { uid, gid });
    }
    found.ok_or_else(|| RuntimeError::new(format!("main user_not_found: {user}")))
}

fn parse_main_passwd_id(
    value: &str,
    line: usize,
    reason: &'static str,
) -> Result<u32, RuntimeError> {
    let id = value.parse::<u32>().map_err(|_| {
        RuntimeError::new(format!("main invalid_passwd_entry: line {line}: {reason}"))
    })?;
    if id == u32::MAX {
        return Err(RuntimeError::new(format!(
            "main invalid_passwd_entry: line {line}: {reason}"
        )));
    }
    Ok(id)
}

fn flatten(spec: Spec) -> Result<Vec<LaunchProcess>, RuntimeError> {
    let main_users = resolve_main_users(&spec.main)?;
    let mut result = Vec::with_capacity(spec.sidecars.len() + spec.main.len());
    result.extend(spec.sidecars.into_iter().map(from_sidecar));
    result.extend(
        spec.main
            .into_iter()
            .map(|process| from_main(process, &main_users)),
    );
    Ok(result)
}

fn from_sidecar(process: SidecarProcess) -> LaunchProcess {
    LaunchProcess {
        name: process.name,
        kind: process.kind,
        command: process.command,
        env: process.env,
        workdir: process.workdir,
        readiness_probe: process.readiness_probe,
        completion_timeout_ms: process.completion_timeout_ms,
        source: ProcessSource::Sidecar {
            rootfs: process.rootfs,
            overlay_device: process.overlay_device,
            standard_mounts: process.standard_mounts,
            disk_mounts: process.disk_mounts,
            binds: process.binds,
            user: process.user,
        },
    }
}

fn from_main(
    process: MainProcess,
    resolved_users: &BTreeMap<String, NumericUser>,
) -> LaunchProcess {
    let user = match process.user {
        Some(ProcessUser::Named(user)) => Some(
            *resolved_users
                .get(&user.name)
                .expect("all named main users are resolved before launch"),
        ),
        Some(ProcessUser::Numeric(user)) => Some(user),
        None => None,
    };
    LaunchProcess {
        name: process.name,
        kind: process.kind,
        command: process.command,
        env: process.env,
        workdir: process.workdir,
        readiness_probe: process.readiness_probe,
        completion_timeout_ms: process.completion_timeout_ms,
        source: ProcessSource::Main { user },
    }
}

fn build_command(process: &LaunchProcess, sandrun: &Path) -> Command {
    let mut command = match &process.source {
        ProcessSource::Main { .. } => {
            let mut command = Command::new(&process.command[0]);
            command.args(&process.command[1..]);
            if !process.workdir.is_empty() {
                command.current_dir(&process.workdir);
            }
            command
        }
        ProcessSource::Sidecar {
            rootfs,
            overlay_device,
            standard_mounts,
            disk_mounts,
            binds,
            user,
        } => {
            let mut command = Command::new(sandrun);
            command.args(["--rootfs", rootfs]);
            if let Some(device) = overlay_device {
                command.args(["--overlay-device", device]);
            }
            command.args(["--overlay-id", &process.name]);
            if *standard_mounts {
                command.arg("--standard-mounts");
            }
            for target in disk_mounts {
                command.args(["--disk-mount", target]);
            }
            for bind in binds {
                command.arg(if bind.readonly { "--ro-bind" } else { "--bind" });
                command.args([&bind.source, &bind.target]);
            }
            if !process.workdir.is_empty() {
                command.args(["--workdir", &process.workdir]);
            }
            if let Some(user) = user {
                match user {
                    ProcessUser::Named(user) => {
                        command.args(["--user", &user.name]);
                    }
                    ProcessUser::Numeric(user) => {
                        command
                            .arg("--uid")
                            .arg(user.uid.to_string())
                            .arg("--gid")
                            .arg(user.gid.to_string());
                    }
                }
            }
            command.arg("--");
            command.args(&process.command);
            command.env_clear();
            command
        }
    };
    command
        .envs(&process.env)
        .env_remove(SPEC_ENVIRONMENT)
        .stdin(Stdio::null())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit());
    command
}

fn sandrun_path() -> Result<PathBuf, RuntimeError> {
    let executable = std::env::current_exe()
        .map_err(|error| RuntimeError::new(format!("resolve campd executable: {error}")))?;
    let parent = executable
        .parent()
        .ok_or_else(|| RuntimeError::new("campd executable has no parent directory"))?;
    Ok(parent.join("sandrun"))
}

fn install_runtime_contract() -> Result<(), RuntimeError> {
    if unsafe { libc::prctl(libc::PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) } != 0 {
        return Err(RuntimeError::new(format!(
            "set child subreaper: {}",
            io::Error::last_os_error()
        )));
    }
    for signal in [libc::SIGTERM, libc::SIGINT, libc::SIGHUP, libc::SIGQUIT] {
        let mut action: libc::sigaction = unsafe { std::mem::zeroed() };
        action.sa_sigaction = signal_handler as usize;
        action.sa_flags = 0;
        unsafe {
            libc::sigemptyset(&mut action.sa_mask);
        }
        if unsafe { libc::sigaction(signal, &action, std::ptr::null_mut()) } != 0 {
            return Err(RuntimeError::new(format!(
                "install signal handler {signal}: {}",
                io::Error::last_os_error()
            )));
        }
    }
    Ok(())
}

extern "C" fn signal_handler(signal: libc::c_int) {
    RECEIVED_SIGNAL.store(signal, Ordering::Release);
}

fn received_signal() -> Option<i32> {
    match RECEIVED_SIGNAL.load(Ordering::Acquire) {
        0 => None,
        signal => Some(signal),
    }
}

fn apply_main_identity(user: NumericUser) -> io::Result<()> {
    if unsafe { libc::geteuid() } != 0 {
        return Err(io::Error::from_raw_os_error(libc::EPERM));
    }
    let non_root = user.uid != 0;
    if non_root
        && unsafe {
            libc::prctl(
                libc::PR_CAP_AMBIENT,
                libc::PR_CAP_AMBIENT_CLEAR_ALL,
                0,
                0,
                0,
            )
        } != 0
    {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::setgroups(0, ptr::null()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::setresgid(user.gid, user.gid, user.gid) } != 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::setresuid(user.uid, user.uid, user.uid) } != 0 {
        return Err(io::Error::last_os_error());
    }
    if non_root {
        let mut header = CapabilityHeader {
            version: LINUX_CAPABILITY_VERSION_3,
            pid: 0,
        };
        let mut data = [CapabilityData {
            effective: 0,
            permitted: 0,
            inheritable: 0,
        }; 2];
        if unsafe { libc::syscall(libc::SYS_capset, &mut header, data.as_mut_ptr()) } != 0 {
            return Err(io::Error::last_os_error());
        }
        if unsafe { libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) } != 0 {
            return Err(io::Error::last_os_error());
        }
    }
    if unsafe { libc::getuid() } != user.uid
        || unsafe { libc::geteuid() } != user.uid
        || unsafe { libc::getgid() } != user.gid
        || unsafe { libc::getegid() } != user.gid
    {
        return Err(io::Error::from_raw_os_error(libc::EPERM));
    }
    Ok(())
}

fn spawn_probe_worker(
    process_index: usize,
    process_name: String,
    probe: Probe,
    cancel: Arc<AtomicBool>,
    sender: Sender<ProbeEvent>,
) -> io::Result<()> {
    thread::Builder::new()
        .name(format!("campd-probe-{process_name}"))
        .spawn(move || {
            while !cancel.load(Ordering::Acquire) {
                let deadline = Instant::now() + Duration::from_millis(probe.timeout_ms);
                let success = http_probe(&probe, deadline);
                if sender
                    .send(ProbeEvent {
                        process_index,
                        success,
                    })
                    .is_err()
                {
                    return;
                }
                sleep_with_cancel(Duration::from_millis(probe.period_ms), &cancel);
            }
        })?;
    Ok(())
}

fn http_probe(probe: &Probe, deadline: Instant) -> bool {
    let Some(connect_timeout) = remaining_until(deadline) else {
        return false;
    };
    let address = SocketAddr::new(IpAddr::V4(Ipv4Addr::LOCALHOST), probe.port);
    let Ok(mut stream) = TcpStream::connect_timeout(&address, connect_timeout) else {
        return false;
    };
    let Some(write_timeout) = remaining_until(deadline) else {
        return false;
    };
    if stream.set_write_timeout(Some(write_timeout)).is_err() {
        return false;
    }
    let request = format!(
        "GET {} HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n",
        probe.path
    );
    if stream.write_all(request.as_bytes()).is_err() {
        return false;
    }
    let mut response = [0_u8; 128];
    let mut size = 0;
    let line_end = loop {
        let Some(read_timeout) = remaining_until(deadline) else {
            return false;
        };
        if stream.set_read_timeout(Some(read_timeout)).is_err() {
            return false;
        }
        let Ok(read) = stream.read(&mut response[size..]) else {
            return false;
        };
        if read == 0 {
            return false;
        }
        size += read;
        if let Some(line_end) = response[..size].iter().position(|byte| *byte == b'\n') {
            break line_end;
        }
        if size == response.len() {
            return false;
        }
    };
    let first_line = String::from_utf8_lossy(&response[..line_end]);
    let mut parts = first_line.trim_end_matches('\r').split_whitespace();
    let version = parts.next().unwrap_or_default();
    let status = parts.next().unwrap_or_default();
    matches!(version, "HTTP/1.0" | "HTTP/1.1")
        && status.len() == 3
        && status.bytes().all(|byte| byte.is_ascii_digit())
        && status
            .parse::<u16>()
            .is_ok_and(|status| (200..400).contains(&status))
}

fn remaining_until(deadline: Instant) -> Option<Duration> {
    let remaining = deadline.saturating_duration_since(Instant::now());
    (!remaining.is_zero()).then_some(remaining)
}

fn sleep_with_cancel(duration: Duration, cancel: &AtomicBool) {
    let deadline = Instant::now() + duration;
    while !cancel.load(Ordering::Acquire) {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            break;
        }
        thread::sleep(remaining.min(POLL_INTERVAL));
    }
}

fn wait_duration(deadline: Instant) -> Duration {
    deadline
        .saturating_duration_since(Instant::now())
        .min(POLL_INTERVAL)
}

fn signal_group(group: libc::pid_t, signal: i32) {
    if unsafe { libc::kill(-group, signal) } != 0 {
        let error = io::Error::last_os_error();
        if error.raw_os_error() != Some(libc::ESRCH) {
            eprintln!("campd: signal failed group={group} signal={signal} error={error}");
        }
    }
}

fn terminate_groups(groups: &HashSet<libc::pid_t>, signal: i32) {
    let mut remaining = groups.clone();
    for group in groups {
        signal_group(*group, signal);
    }
    let deadline = Instant::now() + SHUTDOWN_GRACE;
    while !remaining.is_empty() && Instant::now() < deadline {
        reap_any_available();
        remaining.retain(|group| process_group_exists(*group));
        if !remaining.is_empty() {
            thread::sleep(POLL_INTERVAL);
        }
    }
    for group in &remaining {
        signal_group(*group, libc::SIGKILL);
    }
    let deadline = Instant::now() + KILL_GRACE;
    while !remaining.is_empty() && Instant::now() < deadline {
        reap_any_available();
        remaining.retain(|group| process_group_exists(*group));
        if !remaining.is_empty() {
            thread::sleep(POLL_INTERVAL);
        }
    }
    if !remaining.is_empty() {
        eprintln!("campd: process groups survived SIGKILL groups={remaining:?}");
    }
}

fn reap_any_available() {
    loop {
        let mut status = 0;
        let pid = unsafe { libc::waitpid(-1, &mut status, libc::WNOHANG) };
        if pid <= 0 {
            break;
        }
    }
}

fn reap_pid(pid: libc::pid_t) {
    loop {
        let mut status = 0;
        if unsafe { libc::waitpid(pid, &mut status, 0) } >= 0 {
            return;
        }
        if io::Error::last_os_error().kind() != io::ErrorKind::Interrupted {
            return;
        }
    }
}

fn process_group_exists(group: libc::pid_t) -> bool {
    if unsafe { libc::kill(-group, 0) } == 0 {
        return true;
    }
    io::Error::last_os_error().raw_os_error() == Some(libc::EPERM)
}

fn exit_code(status: libc::c_int) -> i32 {
    if libc::WIFEXITED(status) {
        libc::WEXITSTATUS(status)
    } else if libc::WIFSIGNALED(status) {
        128 + libc::WTERMSIG(status)
    } else {
        1
    }
}

fn display_status(status: libc::c_int) -> String {
    if libc::WIFEXITED(status) {
        format!("exit({})", libc::WEXITSTATUS(status))
    } else if libc::WIFSIGNALED(status) {
        format!("signal({})", libc::WTERMSIG(status))
    } else {
        format!("status({status})")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sidecar_command_is_built_by_campd() {
        let process = LaunchProcess {
            name: "proxy".into(),
            kind: ProcessKind::Service,
            command: vec!["/opt/proxy/server".into(), "--listen".into()],
            env: BTreeMap::new(),
            workdir: "/opt/proxy".into(),
            readiness_probe: None,
            completion_timeout_ms: None,
            source: ProcessSource::Sidecar {
                rootfs: "/mnt/proxy".into(),
                overlay_device: Some("/dev/vda".into()),
                standard_mounts: true,
                disk_mounts: vec!["/var/lib/docker".into()],
                binds: vec![Bind {
                    source: "/share".into(),
                    target: "/mnt/share".into(),
                    readonly: false,
                }],
                user: Some(ProcessUser::Named(crate::spec::NamedUser {
                    name: "app".into(),
                })),
            },
        };
        let command = build_command(&process, Path::new("/runtime/sandrun"));
        assert_eq!(command.get_program(), "/runtime/sandrun");
        let arguments = command
            .get_args()
            .map(|argument| argument.to_string_lossy().into_owned())
            .collect::<Vec<_>>();
        assert_eq!(
            arguments,
            [
                "--rootfs",
                "/mnt/proxy",
                "--overlay-device",
                "/dev/vda",
                "--overlay-id",
                "proxy",
                "--standard-mounts",
                "--disk-mount",
                "/var/lib/docker",
                "--bind",
                "/share",
                "/mnt/share",
                "--workdir",
                "/opt/proxy",
                "--user",
                "app",
                "--",
                "/opt/proxy/server",
                "--listen",
            ]
        );
    }

    #[test]
    fn numeric_sidecar_user_is_passed_without_passwd_lookup() {
        let process = LaunchProcess {
            name: "worker".into(),
            kind: ProcessKind::Service,
            command: vec!["/bin/worker".into()],
            env: BTreeMap::new(),
            workdir: String::new(),
            readiness_probe: None,
            completion_timeout_ms: None,
            source: ProcessSource::Sidecar {
                rootfs: "/mnt/worker".into(),
                overlay_device: None,
                standard_mounts: false,
                disk_mounts: Vec::new(),
                binds: Vec::new(),
                user: Some(ProcessUser::Numeric(NumericUser {
                    uid: 65_532,
                    gid: 65_531,
                })),
            },
        };
        let command = build_command(&process, Path::new("/runtime/sandrun"));
        let arguments = command
            .get_args()
            .map(|argument| argument.to_string_lossy().into_owned())
            .collect::<Vec<_>>();
        assert_eq!(
            arguments,
            [
                "--rootfs",
                "/mnt/worker",
                "--overlay-id",
                "worker",
                "--uid",
                "65532",
                "--gid",
                "65531",
                "--",
                "/bin/worker",
            ]
        );
    }

    #[test]
    fn main_passwd_parser_is_strict() {
        let contents = concat!(
            "root:x:0:0:root:/root:/bin/sh\n",
            "app:x:65532:65531:app:/home/app:/bin/false\n",
        );
        assert_eq!(
            parse_main_passwd(contents, "app").unwrap(),
            NumericUser {
                uid: 65_532,
                gid: 65_531,
            }
        );
        assert!(
            parse_main_passwd(contents, "missing")
                .unwrap_err()
                .to_string()
                .contains("user_not_found")
        );
        assert!(
            parse_main_passwd(
                "app:x:65532:65532:a:/home/app:/bin/false\napp:x:1:1:b:/:/bin/false\n",
                "app",
            )
            .unwrap_err()
            .to_string()
            .contains("duplicated")
        );
        assert!(
            parse_main_passwd("app:x:not-a-uid:1:a:/:/bin/false\n", "app")
                .unwrap_err()
                .to_string()
                .contains("UID is invalid")
        );
    }

    #[test]
    fn readiness_thresholds_fail_and_recover() {
        let state = ProbeState {
            health: ProbeHealth::Starting,
            consecutive_successes: 0,
            consecutive_failures: 0,
            success_threshold: 2,
            failure_threshold: 2,
        };
        assert_eq!(state.health, ProbeHealth::Starting);

        let mut state = state;
        assert_eq!(state.observe(true), None);
        assert_eq!(
            state.observe(true),
            Some((ProbeHealth::Starting, ProbeHealth::Ready))
        );
        assert_eq!(state.observe(false), None);
        assert_eq!(
            state.observe(false),
            Some((ProbeHealth::Ready, ProbeHealth::Unready))
        );
        assert_eq!(state.observe(true), None);
        assert_eq!(
            state.observe(true),
            Some((ProbeHealth::Unready, ProbeHealth::Ready))
        );
    }
}
