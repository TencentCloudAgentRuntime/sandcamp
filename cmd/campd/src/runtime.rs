use crate::SPEC_ENVIRONMENT;
use crate::ready::ReadyState;
use crate::spec::{Probe, Process, ProcessUser, Spec};
use std::collections::HashSet;
use std::error::Error;
use std::fmt;
use std::io::{self, Read, Write};
use std::net::{IpAddr, Ipv4Addr, SocketAddr, TcpStream};
use std::os::unix::process::CommandExt;
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicI32, Ordering};
use std::thread;
use std::time::{Duration, Instant};

const SHUTDOWN_GRACE: Duration = Duration::from_secs(5);
const POLL_INTERVAL: Duration = Duration::from_millis(50);
static RECEIVED_SIGNAL: AtomicI32 = AtomicI32::new(0);

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

#[derive(Debug)]
struct ManagedProcess {
    name: String,
    pid: libc::pid_t,
    main: bool,
}

pub fn run(spec: Spec, ready: ReadyState) -> Result<i32, RuntimeError> {
    install_runtime_contract()?;
    let mut children = Vec::with_capacity(spec.processes.len());

    for process in &spec.processes {
        if let Some(signal) = received_signal() {
            ready.set(false);
            terminate(&children, signal);
            return Ok(128 + signal);
        }
        let child = match spawn(process) {
            Ok(child) => child,
            Err(error) => {
                ready.set(false);
                terminate(&children, libc::SIGTERM);
                return Err(RuntimeError::new(format!(
                    "failed to start {}: {error}",
                    process.name
                )));
            }
        };
        eprintln!("campd: started process={} pid={}", child.name, child.pid);
        children.push(child);
        if let Some(probe) = &process.startup_probe {
            if let Err(error) = wait_until_ready(&children, process, probe) {
                ready.set(false);
                if let Some(signal) = received_signal() {
                    terminate(&children, signal);
                    return Ok(128 + signal);
                }
                terminate(&children, libc::SIGTERM);
                return Err(error);
            }
            eprintln!("campd: startup probe passed process={}", process.name);
        }
    }

    if let Some(signal) = received_signal() {
        ready.set(false);
        terminate(&children, signal);
        return Ok(128 + signal);
    }
    let exited = match first_exited(&children) {
        Ok(exited) => exited,
        Err(error) => {
            ready.set(false);
            if let Some(signal) = received_signal() {
                terminate(&children, signal);
                return Ok(128 + signal);
            }
            terminate(&children, libc::SIGTERM);
            return Err(error);
        }
    };
    if let Some((child, status)) = exited {
        ready.set(false);
        terminate(&children, libc::SIGTERM);
        return Err(RuntimeError::new(format!(
            "process {} exited before readiness with {}",
            child.name,
            display_status(status)
        )));
    }
    ready.set(true);
    let result = supervise(&children, &ready);
    ready.set(false);
    result
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

fn spawn(process: &Process) -> io::Result<ManagedProcess> {
    let mut command = Command::new(&process.argv[0]);
    if !process.main {
        // Image Volume sidecars receive only their declared environment.
        // Inheriting campd's environment would leak main-image variables and
        // credentials into every sidecar.
        command.env_clear();
    }
    command
        .args(&process.argv[1..])
        .envs(&process.env)
        .env_remove(SPEC_ENVIRONMENT)
        .stdin(Stdio::null())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit());
    if !process.workdir.is_empty() {
        command.current_dir(&process.workdir);
    }
    let user = process.user;
    unsafe {
        command.pre_exec(move || {
            if libc::setpgid(0, 0) != 0 {
                return Err(io::Error::last_os_error());
            }
            if let Some(user) = user {
                drop_process_privileges(user)?;
            }
            Ok(())
        });
    }
    let child = command.spawn()?;
    let pid = child.id() as libc::pid_t;
    drop(child);
    Ok(ManagedProcess {
        name: process.name.clone(),
        pid,
        main: process.main,
    })
}

fn drop_process_privileges(user: ProcessUser) -> io::Result<()> {
    if unsafe { libc::geteuid() } != 0 {
        return Err(io::Error::from_raw_os_error(libc::EPERM));
    }
    if unsafe { libc::setgroups(0, std::ptr::null()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::setresgid(user.gid, user.gid, user.gid) } != 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::setresuid(user.uid, user.uid, user.uid) } != 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) } != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

fn wait_until_ready(
    children: &[ManagedProcess],
    process: &Process,
    probe: &Probe,
) -> Result<(), RuntimeError> {
    let deadline = Instant::now() + Duration::from_millis(probe.ready_timeout_ms);
    loop {
        if let Some(signal) = received_signal() {
            return Err(RuntimeError::new(format!(
                "received signal {signal} while starting {}",
                process.name
            )));
        }
        if let Some((child, status)) = first_exited(children)? {
            return Err(RuntimeError::new(format!(
                "process {} exited during startup with {}",
                child.name,
                display_status(status)
            )));
        }
        let now = Instant::now();
        if now >= deadline {
            return Err(RuntimeError::new(format!(
                "startup probe timed out for {} after {}ms",
                process.name, probe.ready_timeout_ms
            )));
        }
        let remaining = deadline.saturating_duration_since(now);
        let attempt_deadline = now + Duration::from_millis(probe.timeout_ms).min(remaining);
        if http_probe(probe, attempt_deadline) {
            return Ok(());
        }
        sleep_interruptibly(
            Duration::from_millis(probe.period_ms)
                .min(deadline.saturating_duration_since(Instant::now())),
        );
    }
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

fn sleep_interruptibly(duration: Duration) {
    let deadline = Instant::now() + duration;
    while received_signal().is_none() {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            break;
        }
        thread::sleep(remaining.min(POLL_INTERVAL));
    }
}

fn supervise(children: &[ManagedProcess], ready: &ReadyState) -> Result<i32, RuntimeError> {
    loop {
        if let Some(signal) = received_signal() {
            ready.set(false);
            terminate(children, signal);
            eprintln!("campd: received signal={signal}");
            return Ok(128 + signal);
        }

        let mut status = 0;
        let pid = unsafe { libc::waitpid(-1, &mut status, 0) };
        if pid > 0 {
            if let Some(child) = children.iter().find(|child| child.pid == pid) {
                ready.set(false);
                terminate(children, libc::SIGTERM);
                eprintln!(
                    "campd: process exited process={} pid={} status={}",
                    child.name,
                    pid,
                    display_status(status)
                );
                return Ok(if child.main { exit_code(status) } else { 1 });
            }
            continue;
        }
        if pid == -1 {
            let error = io::Error::last_os_error();
            if error.kind() == io::ErrorKind::Interrupted {
                continue;
            }
            return Err(RuntimeError::new(format!("wait for processes: {error}")));
        }
    }
}

fn first_exited(
    children: &[ManagedProcess],
) -> Result<Option<(&ManagedProcess, libc::c_int)>, RuntimeError> {
    for child in children {
        let mut status = 0;
        let result = unsafe { libc::waitpid(child.pid, &mut status, libc::WNOHANG) };
        if result == child.pid {
            return Ok(Some((child, status)));
        }
        if result == -1 {
            let error = io::Error::last_os_error();
            if error.raw_os_error() != Some(libc::ECHILD) {
                return Err(RuntimeError::new(format!(
                    "inspect process {}: {error}",
                    child.name
                )));
            }
        }
    }
    Ok(None)
}

fn terminate(children: &[ManagedProcess], signal: i32) {
    let mut remaining_groups = children
        .iter()
        .map(|child| child.pid)
        .collect::<HashSet<_>>();
    for child in children {
        if unsafe { libc::kill(-child.pid, signal) } != 0 {
            let error = io::Error::last_os_error();
            if error.raw_os_error() != Some(libc::ESRCH) {
                eprintln!(
                    "campd: signal failed process={} signal={} error={}",
                    child.name, signal, error
                );
            }
        }
    }

    let deadline = Instant::now() + SHUTDOWN_GRACE;
    while !remaining_groups.is_empty() && Instant::now() < deadline {
        reap_available();
        remaining_groups.retain(|group| process_group_exists(*group));
        if !remaining_groups.is_empty() {
            thread::sleep(POLL_INTERVAL);
        }
    }
    for group in &remaining_groups {
        let _ = unsafe { libc::kill(-*group, libc::SIGKILL) };
    }
    let kill_deadline = Instant::now() + Duration::from_secs(1);
    while !remaining_groups.is_empty() && Instant::now() < kill_deadline {
        reap_available();
        remaining_groups.retain(|group| process_group_exists(*group));
        if !remaining_groups.is_empty() {
            thread::sleep(POLL_INTERVAL);
        }
    }
    if !remaining_groups.is_empty() {
        eprintln!(
            "campd: process groups survived SIGKILL groups={:?}",
            remaining_groups
        );
    }
}

fn reap_available() {
    loop {
        let mut status = 0;
        let pid = unsafe { libc::waitpid(-1, &mut status, libc::WNOHANG) };
        if pid <= 0 {
            break;
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
