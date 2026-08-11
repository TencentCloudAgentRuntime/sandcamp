mod spec;

#[cfg(any(target_os = "linux", test))]
mod ready;

#[cfg(target_os = "linux")]
mod runtime;

pub use spec::{
    Bind, MainProcess, NamedUser, NumericUser, Probe, ProcessKind, SidecarProcess, Spec, SpecError,
    decode,
};

use std::error::Error;
use std::io;

#[cfg(target_os = "linux")]
use std::env;

pub const SPEC_ENVIRONMENT: &str = "SANDCAMP_SPEC";
pub const READY_ADDRESS: &str = "0.0.0.0:49982";

#[cfg(target_os = "linux")]
pub fn run() -> Result<i32, Box<dyn Error>> {
    let encoded = env::var(SPEC_ENVIRONMENT)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "SANDCAMP_SPEC is required"))?;
    let spec = decode(&encoded)?;
    let ready_server = ready::ReadyServer::start(READY_ADDRESS)?;
    let exit_code = runtime::run(spec, ready_server.state())?;
    drop(ready_server);
    Ok(exit_code)
}

#[cfg(not(target_os = "linux"))]
pub fn run() -> Result<i32, Box<dyn Error>> {
    Err(io::Error::new(io::ErrorKind::Unsupported, "campd requires Linux").into())
}
