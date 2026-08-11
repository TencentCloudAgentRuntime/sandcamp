#![cfg(target_os = "linux")]

use base64::Engine;
use base64::engine::general_purpose::STANDARD;
use std::fs;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::os::unix::fs::PermissionsExt;
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

static TEST_LOCK: Mutex<()> = Mutex::new(());

#[test]
fn reports_ready_isolates_sidecar_environment_and_forwards_shutdown() {
    let _guard = test_guard();
    let suffix = unique_suffix();
    let sidecar_evidence = format!("/tmp/campd-sidecar-env-{suffix}");
    let main_evidence = format!("/tmp/campd-main-env-{suffix}");
    let spec = serde_json::json!({
        "version": 2,
        "sidecars": [{
            "name": "sidecar",
            "kind": "service",
            "rootfs": "/tmp/fake-sidecar-rootfs",
            "standard_mounts": false,
            "command": [
                "/bin/sh",
                "-c",
                format!(
                    "printf '%s|%s|%s' \"${{SIDECAR_ONLY-unset}}\" \"${{PARENT_ENV-unset}}\" \"${{SANDCAMP_SPEC-unset}}\" > {sidecar_evidence}; exec /bin/sleep 60"
                )
            ],
            "env": {"SIDECAR_ONLY": "from-process"}
        }],
        "main": [{
            "name": "main",
            "kind": "service",
            "command": [
                "/bin/sh",
                "-c",
                format!(
                    "printf '%s|%s|%s' \"${{MAIN_ONLY-unset}}\" \"${{PARENT_ENV-unset}}\" \"${{SANDCAMP_SPEC-unset}}\" > {main_evidence}; exec /bin/sleep 60"
                )
            ],
            "env": {"MAIN_ONLY": "from-process"}
        }]
    });
    let mut campd = RunningCampd::start(&spec, &[("PARENT_ENV", "from-campd")]);

    wait_ready(&mut campd, 200, Duration::from_secs(5));
    assert_eq!(
        fs::read_to_string(&sidecar_evidence).unwrap(),
        "from-process|unset|unset"
    );
    assert_eq!(
        fs::read_to_string(&main_evidence).unwrap(),
        "from-process|from-campd|unset"
    );

    campd.terminate();
    let _ = fs::remove_file(sidecar_evidence);
    let _ = fs::remove_file(main_evidence);
}

#[test]
fn service_exit_marks_unready_without_stopping_peer() {
    let _guard = test_guard();
    let suffix = unique_suffix();
    let peer_pid = format!("/tmp/campd-peer-pid-{suffix}");
    let spec = serde_json::json!({
        "version": 2,
        "sidecars": [],
        "main": [
            {
                "name": "peer",
                "kind": "service",
                "command": ["/bin/sh", "-c", format!("echo $$ > {peer_pid}; exec /bin/sleep 60")]
            },
            {
                "name": "exiting",
                "kind": "service",
                "command": ["/bin/sh", "-c", "sleep 1; exit 0"]
            }
        ]
    });
    let mut campd = RunningCampd::start(&spec, &[]);

    wait_ready(&mut campd, 200, Duration::from_secs(5));
    wait_ready(&mut campd, 503, Duration::from_secs(3));
    campd.assert_running();
    let peer: libc::pid_t = fs::read_to_string(&peer_pid)
        .unwrap()
        .trim()
        .parse()
        .unwrap();
    assert_eq!(unsafe { libc::kill(peer, 0) }, 0, "peer was stopped");

    campd.terminate();
    let _ = fs::remove_file(peer_pid);
}

#[test]
fn continuous_probe_can_fail_recover_and_cannot_mask_process_exit() {
    let _guard = test_guard();
    let server = ProbeServer::start();
    let suffix = unique_suffix();
    let service_pid = format!("/tmp/campd-probed-pid-{suffix}");
    let spec = serde_json::json!({
        "version": 2,
        "sidecars": [],
        "main": [{
            "name": "probed",
            "kind": "service",
            "command": ["/bin/sh", "-c", format!("echo $$ > {service_pid}; exec /bin/sleep 60")],
            "readiness_probe": {
                "path": "/healthz",
                "port": server.port(),
                "startup_timeout_ms": 2000,
                "period_ms": 100,
                "timeout_ms": 100,
                "failure_threshold": 2,
                "success_threshold": 2
            }
        }]
    });
    let mut campd = RunningCampd::start(&spec, &[]);

    wait_ready(&mut campd, 200, Duration::from_secs(5));
    server.set_healthy(false);
    wait_ready(&mut campd, 503, Duration::from_secs(3));
    campd.assert_running();
    server.set_healthy(true);
    wait_ready(&mut campd, 200, Duration::from_secs(3));

    let pid: libc::pid_t = fs::read_to_string(&service_pid)
        .unwrap()
        .trim()
        .parse()
        .unwrap();
    assert_eq!(unsafe { libc::kill(pid, libc::SIGTERM) }, 0);
    wait_ready(&mut campd, 503, Duration::from_secs(3));
    campd.assert_running();

    campd.terminate();
    let _ = fs::remove_file(service_pid);
}

#[test]
fn run_to_completion_gates_following_service_and_failure_stops_sequence() {
    let _guard = test_guard();
    let suffix = unique_suffix();
    let job_evidence = format!("/tmp/campd-job-{suffix}");
    let service_evidence = format!("/tmp/campd-after-job-{suffix}");
    let success = serde_json::json!({
        "version": 2,
        "sidecars": [],
        "main": [
            {
                "name": "prepare",
                "kind": "run-to-completion",
                "command": ["/bin/sh", "-c", format!("sleep 0.2; echo ready > {job_evidence}")],
                "completion_timeout_ms": 2000
            },
            {
                "name": "service",
                "kind": "service",
                "command": ["/bin/sh", "-c", format!("test -s {job_evidence}; echo started > {service_evidence}; exec /bin/sleep 60")]
            }
        ]
    });
    let mut campd = RunningCampd::start(&success, &[]);
    wait_ready(&mut campd, 200, Duration::from_secs(5));
    assert_eq!(fs::read_to_string(&service_evidence).unwrap(), "started\n");
    campd.terminate();

    let _ = fs::remove_file(&service_evidence);
    let failure = serde_json::json!({
        "version": 2,
        "sidecars": [],
        "main": [
            {
                "name": "prepare",
                "kind": "run-to-completion",
                "command": ["/bin/sh", "-c", "exit 7"],
                "completion_timeout_ms": 2000
            },
            {
                "name": "never-started",
                "kind": "service",
                "command": ["/bin/sh", "-c", format!("echo unexpected > {service_evidence}; exec /bin/sleep 60")]
            }
        ]
    });
    let mut campd = RunningCampd::start(&failure, &[]);
    wait_ready(&mut campd, 503, Duration::from_secs(2));
    thread::sleep(Duration::from_millis(300));
    campd.assert_running();
    assert!(!PathBuf::from(&service_evidence).exists());
    campd.terminate();

    let _ = fs::remove_file(job_evidence);
    let _ = fs::remove_file(service_evidence);
}

#[test]
fn run_to_completion_timeout_cleans_its_group_and_keeps_campd_unready() {
    let _guard = test_guard();
    let suffix = unique_suffix();
    let job_pid = format!("/tmp/campd-timeout-job-{suffix}");
    let next_evidence = format!("/tmp/campd-timeout-next-{suffix}");
    let spec = serde_json::json!({
        "version": 2,
        "sidecars": [],
        "main": [
            {
                "name": "stuck-job",
                "kind": "run-to-completion",
                "command": ["/bin/sh", "-c", format!("trap '' TERM; echo $$ > {job_pid}; exec /bin/sleep 60")],
                "completion_timeout_ms": 300
            },
            {
                "name": "never-started",
                "kind": "service",
                "command": ["/bin/sh", "-c", format!("echo unexpected > {next_evidence}; exec /bin/sleep 60")]
            }
        ]
    });
    let mut campd = RunningCampd::start(&spec, &[]);

    wait_for_file(&mut campd, &job_pid, Duration::from_secs(2));
    wait_ready(&mut campd, 503, Duration::from_secs(2));
    let pid: libc::pid_t = fs::read_to_string(&job_pid)
        .unwrap()
        .trim()
        .parse()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(7);
    while unsafe { libc::kill(pid, 0) } == 0 && Instant::now() < deadline {
        thread::sleep(Duration::from_millis(50));
    }
    assert_ne!(
        unsafe { libc::kill(pid, 0) },
        0,
        "timed-out job survived cleanup"
    );
    campd.assert_running();
    assert!(!PathBuf::from(&next_evidence).exists());

    campd.terminate();
    let _ = fs::remove_file(job_pid);
    let _ = fs::remove_file(next_evidence);
}

fn test_guard() -> std::sync::MutexGuard<'static, ()> {
    TEST_LOCK
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

fn unique_suffix() -> String {
    format!("{}-{:?}", std::process::id(), thread::current().id()).replace(['(', ')', ' '], "")
}

struct RunningCampd {
    child: Child,
    runtime_dir: PathBuf,
    stopped: bool,
}

impl RunningCampd {
    fn start(spec: &serde_json::Value, environment: &[(&str, &str)]) -> Self {
        let runtime_dir = std::env::temp_dir().join(format!("campd-runtime-{}", unique_suffix()));
        let _ = fs::remove_dir_all(&runtime_dir);
        fs::create_dir(&runtime_dir).unwrap();
        let campd_path = runtime_dir.join("campd");
        fs::copy(env!("CARGO_BIN_EXE_campd"), &campd_path).unwrap();
        let sandrun_path = runtime_dir.join("sandrun");
        fs::write(
            &sandrun_path,
            "#!/bin/sh\nwhile [ \"$1\" != \"--\" ]; do shift; done\nshift\nexec \"$@\"\n",
        )
        .unwrap();
        fs::set_permissions(&sandrun_path, fs::Permissions::from_mode(0o755)).unwrap();

        let encoded = STANDARD.encode(serde_json::to_vec(spec).unwrap());
        let mut command = Command::new(&campd_path);
        command
            .env("SANDCAMP_SPEC", encoded)
            .stdout(Stdio::null())
            .stderr(Stdio::inherit());
        for (name, value) in environment {
            command.env(name, value);
        }
        let child = command.spawn().unwrap();
        Self {
            child,
            runtime_dir,
            stopped: false,
        }
    }

    fn assert_running(&mut self) {
        if let Some(status) = self.child.try_wait().unwrap() {
            panic!("campd exited unexpectedly: {status}");
        }
    }

    fn terminate(&mut self) {
        if self.stopped {
            return;
        }
        assert_eq!(
            unsafe { libc::kill(self.child.id() as libc::pid_t, libc::SIGTERM) },
            0
        );
        let deadline = Instant::now() + Duration::from_secs(7);
        loop {
            if let Some(status) = self.child.try_wait().unwrap() {
                assert_eq!(status.code(), Some(128 + libc::SIGTERM));
                self.stopped = true;
                return;
            }
            assert!(Instant::now() < deadline, "campd did not stop");
            thread::sleep(Duration::from_millis(25));
        }
    }
}

impl Drop for RunningCampd {
    fn drop(&mut self) {
        if !self.stopped {
            let _ = unsafe { libc::kill(self.child.id() as libc::pid_t, libc::SIGKILL) };
            let _ = self.child.wait();
        }
        let _ = fs::remove_dir_all(&self.runtime_dir);
    }
}

fn wait_ready(campd: &mut RunningCampd, expected: u16, timeout: Duration) {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if ready_status() == Some(expected) {
            return;
        }
        campd.assert_running();
        thread::sleep(Duration::from_millis(25));
    }
    panic!(
        "campd readiness did not become {expected}; last status={:?}",
        ready_status()
    );
}

fn wait_for_file(campd: &mut RunningCampd, path: &str, timeout: Duration) {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if PathBuf::from(path).exists() {
            return;
        }
        campd.assert_running();
        thread::sleep(Duration::from_millis(25));
    }
    panic!("campd process did not create {path}");
}

fn ready_status() -> Option<u16> {
    let mut stream = TcpStream::connect_timeout(
        &"127.0.0.1:49982".parse().unwrap(),
        Duration::from_millis(100),
    )
    .ok()?;
    stream
        .write_all(b"GET /ready HTTP/1.1\r\nHost: localhost\r\n\r\n")
        .ok()?;
    stream
        .set_read_timeout(Some(Duration::from_millis(100)))
        .ok()?;
    let mut response = [0_u8; 128];
    let size = stream.read(&mut response).ok()?;
    String::from_utf8_lossy(&response[..size])
        .lines()
        .next()?
        .split_whitespace()
        .nth(1)?
        .parse()
        .ok()
}

struct ProbeServer {
    address: std::net::SocketAddr,
    healthy: Arc<AtomicBool>,
    stop: Arc<AtomicBool>,
    thread: Option<JoinHandle<()>>,
}

impl ProbeServer {
    fn start() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let address = listener.local_addr().unwrap();
        let healthy = Arc::new(AtomicBool::new(true));
        let stop = Arc::new(AtomicBool::new(false));
        let thread_healthy = Arc::clone(&healthy);
        let thread_stop = Arc::clone(&stop);
        let thread = thread::spawn(move || {
            while !thread_stop.load(Ordering::Acquire) {
                match listener.accept() {
                    Ok((mut stream, _)) => {
                        let _ = stream.set_read_timeout(Some(Duration::from_millis(100)));
                        let mut request = [0_u8; 256];
                        let _ = stream.read(&mut request);
                        let status = if thread_healthy.load(Ordering::Acquire) {
                            "200 OK"
                        } else {
                            "503 Service Unavailable"
                        };
                        let response = format!(
                            "HTTP/1.1 {status}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                        );
                        let _ = stream.write_all(response.as_bytes());
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(10));
                    }
                    Err(error) => panic!("probe server accept: {error}"),
                }
            }
        });
        Self {
            address,
            healthy,
            stop,
            thread: Some(thread),
        }
    }

    fn port(&self) -> u16 {
        self.address.port()
    }

    fn set_healthy(&self, healthy: bool) {
        self.healthy.store(healthy, Ordering::Release);
    }
}

impl Drop for ProbeServer {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        let _ = TcpStream::connect(self.address);
        if let Some(thread) = self.thread.take() {
            thread.join().unwrap();
        }
    }
}
