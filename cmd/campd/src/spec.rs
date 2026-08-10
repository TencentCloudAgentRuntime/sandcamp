use base64::Engine;
use base64::engine::general_purpose::STANDARD;
use serde::Deserialize;
use std::collections::{BTreeMap, HashSet};
use std::error::Error;
use std::fmt;
use std::path::{Component, Path};

const MAX_ENCODED_BYTES: usize = 120 * 1024;
const AGS_READY_TIMEOUT_MS: u64 = 30_000;
const MAX_STARTUP_PROBE_BUDGET_MS: u64 = 25_000;
const CONTROL_PORT: u16 = 49_982;
const SPEC_ENVIRONMENT: &str = "SANDCAMP_SPEC";

#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Spec {
    pub version: u8,
    pub processes: Vec<Process>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Process {
    pub name: String,
    #[serde(default)]
    pub main: bool,
    pub argv: Vec<String>,
    #[serde(default)]
    pub env: BTreeMap<String, String>,
    #[serde(default)]
    pub workdir: String,
    #[serde(default)]
    pub user: Option<ProcessUser>,
    #[serde(default)]
    pub startup_probe: Option<Probe>,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct ProcessUser {
    pub uid: u32,
    pub gid: u32,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Probe {
    pub path: String,
    pub port: u16,
    pub ready_timeout_ms: u64,
    pub period_ms: u64,
    pub timeout_ms: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SpecError(String);

impl SpecError {
    fn new(message: impl Into<String>) -> Self {
        Self(message.into())
    }
}

impl fmt::Display for SpecError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl Error for SpecError {}

pub fn decode(value: &str) -> Result<Spec, SpecError> {
    if value.len() > MAX_ENCODED_BYTES {
        return Err(SpecError::new(format!(
            "encoded declaration exceeds {MAX_ENCODED_BYTES} bytes"
        )));
    }
    let bytes = STANDARD
        .decode(value)
        .map_err(|error| SpecError::new(format!("invalid base64: {error}")))?;
    let spec: Spec = serde_json::from_slice(&bytes)
        .map_err(|error| SpecError::new(format!("invalid JSON: {error}")))?;
    validate(&spec)?;
    Ok(spec)
}

pub fn validate(spec: &Spec) -> Result<(), SpecError> {
    if spec.version != 1 {
        return Err(SpecError::new(format!(
            "unsupported declaration version {}",
            spec.version
        )));
    }
    if spec.processes.is_empty() {
        return Err(SpecError::new("declaration has no processes"));
    }

    let mut names = HashSet::with_capacity(spec.processes.len());
    let mut main_count = 0_u8;
    let mut startup_budget_ms = 0_u64;
    for (index, process) in spec.processes.iter().enumerate() {
        if !valid_name(&process.name) {
            return Err(SpecError::new(format!(
                "process {} has an invalid name",
                process.name
            )));
        }
        if !names.insert(&process.name) {
            return Err(SpecError::new(format!(
                "process {} is duplicated",
                process.name
            )));
        }
        if process.main {
            main_count += 1;
            if index + 1 != spec.processes.len() {
                return Err(SpecError::new("main process must be last"));
            }
        }
        if process.argv.is_empty() || !valid_path(&process.argv[0], false) {
            return Err(SpecError::new(format!(
                "process {} executable must be a clean absolute path",
                process.name
            )));
        }
        if process.argv.iter().any(|value| value.contains('\0')) {
            return Err(SpecError::new(format!(
                "process {} argv contains NUL",
                process.name
            )));
        }
        if !process.workdir.is_empty() && !valid_path(&process.workdir, true) {
            return Err(SpecError::new(format!(
                "process {} workdir must be a clean absolute path",
                process.name
            )));
        }
        if let Some(user) = process.user
            && (user.uid == u32::MAX || user.gid == u32::MAX)
        {
            return Err(SpecError::new(format!(
                "process {} user contains the reserved UID/GID value",
                process.name
            )));
        }
        for (key, value) in &process.env {
            if !valid_environment_name(key) || value.contains('\0') {
                return Err(SpecError::new(format!(
                    "process {} has invalid environment variable {}",
                    process.name, key
                )));
            }
            if key == SPEC_ENVIRONMENT {
                return Err(SpecError::new(format!(
                    "process {} cannot override {}",
                    process.name, SPEC_ENVIRONMENT
                )));
            }
        }
        if let Some(probe) = &process.startup_probe {
            validate_probe(&process.name, probe)?;
            startup_budget_ms = startup_budget_ms
                .checked_add(probe.ready_timeout_ms)
                .ok_or_else(|| SpecError::new("startup probe budget overflow"))?;
        }
    }
    if main_count != 1 {
        return Err(SpecError::new(
            "declaration must contain exactly one main process",
        ));
    }
    if startup_budget_ms > MAX_STARTUP_PROBE_BUDGET_MS {
        return Err(SpecError::new(format!(
            "startup probe budget {startup_budget_ms}ms exceeds the 25000ms limit reserved inside AGS's 30000ms deadline"
        )));
    }
    Ok(())
}

fn validate_probe(name: &str, probe: &Probe) -> Result<(), SpecError> {
    if !valid_http_origin_form(&probe.path) {
        return Err(SpecError::new(format!(
            "process {name} probe path is invalid"
        )));
    }
    if probe.port == 0 {
        return Err(SpecError::new(format!(
            "process {name} probe port is invalid"
        )));
    }
    if probe.port == CONTROL_PORT {
        return Err(SpecError::new(format!(
            "process {name} probe port is reserved by campd"
        )));
    }
    if !(1_000..=AGS_READY_TIMEOUT_MS).contains(&probe.ready_timeout_ms) {
        return Err(SpecError::new(format!(
            "process {name} probe ready timeout is outside AGS limits"
        )));
    }
    for (label, value) in [("period", probe.period_ms), ("timeout", probe.timeout_ms)] {
        if !(100..=AGS_READY_TIMEOUT_MS).contains(&value) {
            return Err(SpecError::new(format!(
                "process {name} probe {label} is outside AGS limits"
            )));
        }
    }
    if probe.timeout_ms > probe.ready_timeout_ms {
        return Err(SpecError::new(format!(
            "process {name} probe timeout exceeds ready timeout"
        )));
    }
    Ok(())
}

fn valid_http_origin_form(value: &str) -> bool {
    let bytes = value.as_bytes();
    if bytes.first() != Some(&b'/') {
        return false;
    }
    let mut index = 0;
    while index < bytes.len() {
        let character = bytes[index];
        if character == b'%' {
            if index + 2 >= bytes.len()
                || !bytes[index + 1].is_ascii_hexdigit()
                || !bytes[index + 2].is_ascii_hexdigit()
            {
                return false;
            }
            index += 3;
            continue;
        }
        if !(character.is_ascii_alphanumeric() || b"-._~!$&'()*+,;=:@/?".contains(&character)) {
            return false;
        }
        index += 1;
    }
    true
}

fn valid_name(value: &str) -> bool {
    let bytes = value.as_bytes();
    !bytes.is_empty()
        && bytes.len() <= 64
        && bytes[0].is_ascii_alphanumeric()
        && bytes
            .iter()
            .all(|value| value.is_ascii_alphanumeric() || matches!(*value, b'_' | b'.' | b'-'))
}

fn valid_environment_name(value: &str) -> bool {
    let mut bytes = value.bytes();
    matches!(bytes.next(), Some(first) if first.is_ascii_alphabetic() || first == b'_')
        && bytes.all(|value| value.is_ascii_alphanumeric() || value == b'_')
}

fn valid_path(value: &str, allow_root: bool) -> bool {
    let path = Path::new(value);
    path.is_absolute()
        && (allow_root || path != Path::new("/"))
        && path
            .components()
            .all(|component| matches!(component, Component::RootDir | Component::Normal(_)))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn valid_spec() -> Spec {
        Spec {
            version: 1,
            processes: vec![
                Process {
                    name: "proxy".into(),
                    main: false,
                    argv: vec!["/bin/proxy".into()],
                    env: BTreeMap::new(),
                    workdir: String::new(),
                    user: None,
                    startup_probe: Some(Probe {
                        path: "/healthz".into(),
                        port: 9200,
                        ready_timeout_ms: 10_000,
                        period_ms: 500,
                        timeout_ms: 1_000,
                    }),
                },
                Process {
                    name: "main".into(),
                    main: true,
                    argv: vec!["/app/server".into()],
                    env: BTreeMap::new(),
                    workdir: "/app".into(),
                    user: None,
                    startup_probe: None,
                },
            ],
        }
    }

    #[test]
    fn accepts_minimal_startup_contract() {
        assert_eq!(validate(&valid_spec()), Ok(()));
    }

    #[test]
    fn accepts_numeric_process_user() {
        let mut spec = valid_spec();
        spec.processes[1].user = Some(ProcessUser {
            uid: 65_532,
            gid: 65_532,
        });
        assert_eq!(validate(&spec), Ok(()));
    }

    #[test]
    fn rejects_multiple_mains_and_nonfinal_main() {
        let mut spec = valid_spec();
        spec.processes[0].main = true;
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("main process")
        );
    }

    #[test]
    fn rejects_features_outside_the_contract() {
        let json = r#"{"version":1,"processes":[{"name":"main","main":true,"argv":["/bin/app"],"restart":"always"}]}"#;
        let value = STANDARD.encode(json);
        assert!(
            decode(&value)
                .unwrap_err()
                .to_string()
                .contains("unknown field")
        );
    }

    #[test]
    fn rejects_encoded_declaration_over_product_limit() {
        let value = "A".repeat(MAX_ENCODED_BYTES + 1);
        assert!(
            decode(&value)
                .unwrap_err()
                .to_string()
                .contains("122880 bytes")
        );
    }

    #[test]
    fn rejects_probe_budget_beyond_ags_deadline() {
        let mut spec = valid_spec();
        spec.processes[1].startup_probe = spec.processes[0].startup_probe.clone();
        spec.processes[0]
            .startup_probe
            .as_mut()
            .unwrap()
            .ready_timeout_ms = 20_001;
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("exceeds the 25000ms limit")
        );
    }

    #[test]
    fn rejects_runtime_environment_and_control_probe() {
        let mut spec = valid_spec();
        spec.processes[0]
            .env
            .insert(SPEC_ENVIRONMENT.into(), "replacement".into());
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("cannot override")
        );

        let mut spec = valid_spec();
        spec.processes[0].startup_probe.as_mut().unwrap().port = CONTROL_PORT;
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("reserved by campd")
        );

        let mut spec = valid_spec();
        spec.processes[0].startup_probe.as_mut().unwrap().path = "/health z#fragment".into();
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("probe path is invalid")
        );

        let mut spec = valid_spec();
        spec.processes[1].user = Some(ProcessUser {
            uid: u32::MAX,
            gid: 65_532,
        });
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("reserved UID/GID")
        );
    }
}
