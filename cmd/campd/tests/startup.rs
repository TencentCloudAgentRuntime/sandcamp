#![cfg(target_os = "linux")]

use base64::Engine;
use base64::engine::general_purpose::STANDARD;
use std::fs;
use std::io::{Read, Write};
use std::net::TcpStream;
use std::process::{Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

#[test]
fn reports_ready_and_forwards_shutdown() {
    let suffix = std::process::id();
    let sidecar_evidence = format!("/tmp/campd-sidecar-env-{suffix}");
    let main_evidence = format!("/tmp/campd-main-env-{suffix}");
    let spec = serde_json::json!({
        "version": 1,
        "processes": [
            {
                "name": "sidecar",
                "argv": [
                    "/bin/sh",
                    "-c",
                    format!(
                        "printf '%s|%s|%s' \"${{SIDECAR_ONLY-unset}}\" \"${{PARENT_ENV-unset}}\" \"${{SANDCAMP_SPEC-unset}}\" > {sidecar_evidence}; exec /bin/sleep 60"
                    )
                ],
                "env": {"SIDECAR_ONLY": "from-process"}
            },
            {
                "name": "main",
                "main": true,
                "argv": [
                    "/bin/sh",
                    "-c",
                    format!(
                        "printf '%s|%s|%s' \"${{MAIN_ONLY-unset}}\" \"${{PARENT_ENV-unset}}\" \"${{SANDCAMP_SPEC-unset}}\" > {main_evidence}; exec /bin/sleep 60"
                    )
                ],
                "env": {"MAIN_ONLY": "from-process"}
            }
        ]
    });
    let encoded = STANDARD.encode(serde_json::to_vec(&spec).unwrap());
    let mut campd = Command::new(env!("CARGO_BIN_EXE_campd"))
        .env("SANDCAMP_SPEC", encoded)
        .env("PARENT_ENV", "from-campd")
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();

    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        if ready_status() == Some(200) {
            break;
        }
        if let Some(status) = campd.try_wait().unwrap() {
            panic!("campd exited before ready: {status}");
        }
        thread::sleep(Duration::from_millis(25));
    }
    assert_eq!(ready_status(), Some(200));
    assert_eq!(
        fs::read_to_string(&sidecar_evidence).unwrap(),
        "from-process|unset|unset"
    );
    assert_eq!(
        fs::read_to_string(&main_evidence).unwrap(),
        "from-process|from-campd|unset"
    );

    assert_eq!(
        unsafe { libc::kill(campd.id() as libc::pid_t, libc::SIGTERM) },
        0
    );
    let deadline = Instant::now() + Duration::from_secs(7);
    loop {
        if let Some(status) = campd.try_wait().unwrap() {
            assert_eq!(status.code(), Some(128 + libc::SIGTERM));
            break;
        }
        assert!(Instant::now() < deadline, "campd did not stop");
        thread::sleep(Duration::from_millis(25));
    }
    let _ = fs::remove_file(sidecar_evidence);
    let _ = fs::remove_file(main_evidence);
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
