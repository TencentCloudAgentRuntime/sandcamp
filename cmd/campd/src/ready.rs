use std::io::{self, Read, Write};
use std::net::{SocketAddr, TcpListener, TcpStream};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread::{self, JoinHandle};
use std::time::Duration;

#[derive(Clone)]
pub struct ReadyState {
    ready: Arc<AtomicBool>,
}

impl ReadyState {
    pub fn set(&self, value: bool) {
        self.ready.store(value, Ordering::Release);
    }

    fn get(&self) -> bool {
        self.ready.load(Ordering::Acquire)
    }
}

pub struct ReadyServer {
    state: ReadyState,
    shutdown: Arc<AtomicBool>,
    thread: Option<JoinHandle<()>>,
    address: SocketAddr,
}

impl ReadyServer {
    pub fn start(address: &str) -> io::Result<Self> {
        let listener = TcpListener::bind(address)?;
        listener.set_nonblocking(true)?;
        let address = listener.local_addr()?;
        let state = ReadyState {
            ready: Arc::new(AtomicBool::new(false)),
        };
        let shutdown = Arc::new(AtomicBool::new(false));
        let thread_state = state.clone();
        let thread_shutdown = Arc::clone(&shutdown);
        let thread = thread::Builder::new()
            .name("campd-ready".into())
            .spawn(move || serve(listener, thread_state, thread_shutdown))?;
        Ok(Self {
            state,
            shutdown,
            thread: Some(thread),
            address,
        })
    }

    pub fn state(&self) -> ReadyState {
        self.state.clone()
    }

    #[cfg(test)]
    pub fn address(&self) -> SocketAddr {
        self.address
    }
}

impl Drop for ReadyServer {
    fn drop(&mut self) {
        self.shutdown.store(true, Ordering::Release);
        let _ = TcpStream::connect(self.address);
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

fn serve(listener: TcpListener, state: ReadyState, shutdown: Arc<AtomicBool>) {
    while !shutdown.load(Ordering::Acquire) {
        match listener.accept() {
            Ok((stream, _)) => respond(stream, state.get()),
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                thread::sleep(Duration::from_millis(20));
            }
            Err(error) => {
                eprintln!("campd: ready endpoint accept failed: {error}");
                thread::sleep(Duration::from_millis(100));
            }
        }
    }
}

fn respond(mut stream: TcpStream, ready: bool) {
    let _ = stream.set_read_timeout(Some(Duration::from_secs(1)));
    let _ = stream.set_write_timeout(Some(Duration::from_secs(1)));
    let mut request = [0_u8; 1024];
    let size = match stream.read(&mut request) {
        Ok(size) => size,
        Err(_) => return,
    };
    let first_line = String::from_utf8_lossy(&request[..size])
        .lines()
        .next()
        .unwrap_or_default()
        .to_owned();
    let mut parts = first_line.split_whitespace();
    let method = parts.next().unwrap_or_default();
    let path = parts.next().unwrap_or_default();
    let (status, body) = if method != "GET" || path != "/ready" {
        ("404 Not Found", "not found\n")
    } else if ready {
        ("200 OK", "ready\n")
    } else {
        ("503 Service Unavailable", "starting\n")
    };
    let response = format!(
        "HTTP/1.1 {status}\r\nContent-Type: text/plain\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    let _ = stream.write_all(response.as_bytes());
}

#[cfg(test)]
mod tests {
    use super::*;

    fn status(address: SocketAddr) -> String {
        let mut stream = TcpStream::connect(address).unwrap();
        stream
            .write_all(b"GET /ready HTTP/1.1\r\nHost: localhost\r\n\r\n")
            .unwrap();
        let mut response = String::new();
        stream.read_to_string(&mut response).unwrap();
        response.lines().next().unwrap().to_owned()
    }

    #[test]
    fn reports_starting_then_ready() {
        let server = ReadyServer::start("127.0.0.1:0").unwrap();
        assert_eq!(status(server.address()), "HTTP/1.1 503 Service Unavailable");
        server.state().set(true);
        assert_eq!(status(server.address()), "HTTP/1.1 200 OK");
    }
}
