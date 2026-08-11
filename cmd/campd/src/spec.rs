use base64::Engine;
use base64::engine::general_purpose::STANDARD;
use serde::Deserialize;
use std::collections::{BTreeMap, HashSet};
use std::error::Error;
use std::fmt;
use std::path::{Component, Path};

const RUNTIME_SPEC_VERSION: u8 = 2;
const MAX_ENCODED_BYTES: usize = 120 * 1024;
const AGS_READY_TIMEOUT_MS: u64 = 30_000;
const MAX_STARTUP_BUDGET_MS: u64 = 25_000;
const CONTROL_PORT: u16 = 49_982;
const SPEC_ENVIRONMENT: &str = "SANDCAMP_SPEC";

#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Spec {
    pub version: u8,
    pub sidecars: Vec<SidecarProcess>,
    pub main: Vec<MainProcess>,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "kebab-case")]
pub enum ProcessKind {
    Service,
    RunToCompletion,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct SidecarProcess {
    pub name: String,
    pub kind: ProcessKind,
    pub rootfs: String,
    #[serde(default)]
    pub overlay_device: Option<String>,
    #[serde(default)]
    pub standard_mounts: bool,
    #[serde(default)]
    pub binds: Vec<Bind>,
    pub command: Vec<String>,
    #[serde(default)]
    pub env: BTreeMap<String, String>,
    #[serde(default)]
    pub workdir: String,
    #[serde(default)]
    pub user: Option<NamedUser>,
    #[serde(default)]
    pub readiness_probe: Option<Probe>,
    #[serde(default)]
    pub completion_timeout_ms: Option<u64>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct MainProcess {
    pub name: String,
    pub kind: ProcessKind,
    pub command: Vec<String>,
    #[serde(default)]
    pub env: BTreeMap<String, String>,
    #[serde(default)]
    pub workdir: String,
    #[serde(default)]
    pub user: Option<NumericUser>,
    #[serde(default)]
    pub readiness_probe: Option<Probe>,
    #[serde(default)]
    pub completion_timeout_ms: Option<u64>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct NamedUser {
    pub name: String,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct NumericUser {
    pub uid: u32,
    pub gid: u32,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Bind {
    pub source: String,
    pub target: String,
    #[serde(default)]
    pub readonly: bool,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Probe {
    pub path: String,
    pub port: u16,
    pub startup_timeout_ms: u64,
    pub period_ms: u64,
    pub timeout_ms: u64,
    pub failure_threshold: u32,
    pub success_threshold: u32,
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
    if spec.version != RUNTIME_SPEC_VERSION {
        return Err(SpecError::new(format!(
            "unsupported declaration version {}",
            spec.version
        )));
    }
    let process_count = spec.sidecars.len() + spec.main.len();
    if process_count == 0 {
        return Err(SpecError::new("declaration has no processes"));
    }

    let mut names = HashSet::with_capacity(process_count);
    let mut startup_budget_ms = 0_u64;
    let mut service_count = 0_usize;

    for process in &spec.sidecars {
        validate_common(
            &process.name,
            process.kind,
            &process.command,
            &process.env,
            &process.workdir,
            process.readiness_probe.as_ref(),
            process.completion_timeout_ms,
            &mut names,
            &mut startup_budget_ms,
            &mut service_count,
        )?;
        if !valid_path(&process.rootfs, false) {
            return Err(SpecError::new(format!(
                "sidecar {} rootfs must be a clean absolute path",
                process.name
            )));
        }
        if let Some(device) = &process.overlay_device
            && !valid_path(device, false)
        {
            return Err(SpecError::new(format!(
                "sidecar {} overlay device must be a clean absolute path",
                process.name
            )));
        }
        if let Some(user) = &process.user
            && !valid_user_name(&user.name)
        {
            return Err(SpecError::new(format!(
                "sidecar {} user name is invalid",
                process.name
            )));
        }
        let mut bind_targets = HashSet::with_capacity(process.binds.len());
        for bind in &process.binds {
            if !valid_path(&bind.source, false) || !valid_path(&bind.target, false) {
                return Err(SpecError::new(format!(
                    "sidecar {} bind paths must be clean and absolute",
                    process.name
                )));
            }
            if !bind_targets.insert(&bind.target) {
                return Err(SpecError::new(format!(
                    "sidecar {} bind target {} is duplicated",
                    process.name, bind.target
                )));
            }
        }
    }

    for process in &spec.main {
        validate_common(
            &process.name,
            process.kind,
            &process.command,
            &process.env,
            &process.workdir,
            process.readiness_probe.as_ref(),
            process.completion_timeout_ms,
            &mut names,
            &mut startup_budget_ms,
            &mut service_count,
        )?;
        if let Some(user) = process.user
            && (user.uid == u32::MAX || user.gid == u32::MAX)
        {
            return Err(SpecError::new(format!(
                "main process {} user contains the reserved UID/GID value",
                process.name
            )));
        }
    }

    if service_count == 0 {
        return Err(SpecError::new(
            "declaration must contain at least one service",
        ));
    }
    if startup_budget_ms > MAX_STARTUP_BUDGET_MS {
        return Err(SpecError::new(format!(
            "startup budget {startup_budget_ms}ms exceeds the 25000ms limit reserved inside AGS's 30000ms deadline"
        )));
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn validate_common(
    name: &str,
    kind: ProcessKind,
    command: &[String],
    env: &BTreeMap<String, String>,
    workdir: &str,
    probe: Option<&Probe>,
    completion_timeout_ms: Option<u64>,
    names: &mut HashSet<String>,
    startup_budget_ms: &mut u64,
    service_count: &mut usize,
) -> Result<(), SpecError> {
    if !valid_name(name) {
        return Err(SpecError::new(format!(
            "process {name} has an invalid name"
        )));
    }
    if !names.insert(name.to_owned()) {
        return Err(SpecError::new(format!("process {name} is duplicated")));
    }
    if command.is_empty() || !valid_path(&command[0], false) {
        return Err(SpecError::new(format!(
            "process {name} executable must be a clean absolute path"
        )));
    }
    if command.iter().any(|value| value.contains('\0')) {
        return Err(SpecError::new(format!(
            "process {name} command contains NUL"
        )));
    }
    if !workdir.is_empty() && !valid_path(workdir, true) {
        return Err(SpecError::new(format!(
            "process {name} workdir must be a clean absolute path"
        )));
    }
    for (key, value) in env {
        if !valid_environment_name(key) || value.contains('\0') {
            return Err(SpecError::new(format!(
                "process {name} has invalid environment variable {key}"
            )));
        }
        if key == SPEC_ENVIRONMENT {
            return Err(SpecError::new(format!(
                "process {name} cannot override {SPEC_ENVIRONMENT}"
            )));
        }
    }

    match kind {
        ProcessKind::Service => {
            *service_count += 1;
            if completion_timeout_ms.is_some() {
                return Err(SpecError::new(format!(
                    "service process {name} cannot set completion timeout"
                )));
            }
            if let Some(probe) = probe {
                validate_probe(name, probe)?;
                add_startup_budget(startup_budget_ms, probe.startup_timeout_ms)?;
            }
        }
        ProcessKind::RunToCompletion => {
            if probe.is_some() {
                return Err(SpecError::new(format!(
                    "run-to-completion process {name} cannot set a readiness probe"
                )));
            }
            let timeout = completion_timeout_ms.ok_or_else(|| {
                SpecError::new(format!(
                    "run-to-completion process {name} requires completion timeout"
                ))
            })?;
            if !(100..=AGS_READY_TIMEOUT_MS).contains(&timeout) {
                return Err(SpecError::new(format!(
                    "process {name} completion timeout is outside AGS limits"
                )));
            }
            add_startup_budget(startup_budget_ms, timeout)?;
        }
    }
    Ok(())
}

fn add_startup_budget(budget: &mut u64, duration: u64) -> Result<(), SpecError> {
    *budget = budget
        .checked_add(duration)
        .ok_or_else(|| SpecError::new("startup budget overflow"))?;
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
    if !(1_000..=AGS_READY_TIMEOUT_MS).contains(&probe.startup_timeout_ms) {
        return Err(SpecError::new(format!(
            "process {name} probe startup timeout is outside AGS limits"
        )));
    }
    for (label, value) in [("period", probe.period_ms), ("timeout", probe.timeout_ms)] {
        if !(100..=AGS_READY_TIMEOUT_MS).contains(&value) {
            return Err(SpecError::new(format!(
                "process {name} probe {label} is outside AGS limits"
            )));
        }
    }
    if probe.timeout_ms > probe.startup_timeout_ms {
        return Err(SpecError::new(format!(
            "process {name} probe timeout exceeds startup timeout"
        )));
    }
    if probe.failure_threshold == 0 || probe.success_threshold == 0 {
        return Err(SpecError::new(format!(
            "process {name} probe thresholds must be positive"
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

fn valid_user_name(value: &str) -> bool {
    let bytes = value.as_bytes();
    !bytes.is_empty()
        && bytes.len() <= 64
        && (bytes[0].is_ascii_alphabetic() || bytes[0] == b'_')
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
    if value.contains('\0') {
        return false;
    }
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

    fn probe(port: u16, startup_timeout_ms: u64) -> Probe {
        Probe {
            path: "/healthz".into(),
            port,
            startup_timeout_ms,
            period_ms: 500,
            timeout_ms: 1_000,
            failure_threshold: 3,
            success_threshold: 1,
        }
    }

    fn valid_spec() -> Spec {
        Spec {
            version: 2,
            sidecars: vec![SidecarProcess {
                name: "proxy".into(),
                kind: ProcessKind::Service,
                rootfs: "/mnt/proxy".into(),
                overlay_device: Some("/dev/vda".into()),
                standard_mounts: true,
                binds: Vec::new(),
                command: vec!["/bin/proxy".into()],
                env: BTreeMap::new(),
                workdir: String::new(),
                user: Some(NamedUser { name: "app".into() }),
                readiness_probe: Some(probe(9200, 10_000)),
                completion_timeout_ms: None,
            }],
            main: vec![
                MainProcess {
                    name: "prepare".into(),
                    kind: ProcessKind::RunToCompletion,
                    command: vec!["/app/prepare".into()],
                    env: BTreeMap::new(),
                    workdir: String::new(),
                    user: None,
                    readiness_probe: None,
                    completion_timeout_ms: Some(1_000),
                },
                MainProcess {
                    name: "api".into(),
                    kind: ProcessKind::Service,
                    command: vec!["/app/server".into()],
                    env: BTreeMap::new(),
                    workdir: "/app".into(),
                    user: Some(NumericUser {
                        uid: 65_532,
                        gid: 65_532,
                    }),
                    readiness_probe: None,
                    completion_timeout_ms: None,
                },
            ],
        }
    }

    #[test]
    fn accepts_grouped_runtime_contract() {
        assert_eq!(validate(&valid_spec()), Ok(()));
    }

    #[test]
    fn accepts_sidecar_only_and_multiple_main_services() {
        let mut sidecar_only = valid_spec();
        sidecar_only.main.clear();
        assert_eq!(validate(&sidecar_only), Ok(()));

        let mut multiple_main = valid_spec();
        multiple_main.main.push(MainProcess {
            name: "worker".into(),
            kind: ProcessKind::Service,
            command: vec!["/app/worker".into()],
            env: BTreeMap::new(),
            workdir: String::new(),
            user: None,
            readiness_probe: None,
            completion_timeout_ms: None,
        });
        assert_eq!(validate(&multiple_main), Ok(()));
    }

    #[test]
    fn rejects_invalid_lifecycle_combinations() {
        let mut spec = valid_spec();
        spec.main[0].completion_timeout_ms = None;
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("requires")
        );

        let mut spec = valid_spec();
        spec.main[0].readiness_probe = Some(probe(8081, 1_000));
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("cannot set a readiness probe")
        );

        let mut spec = valid_spec();
        spec.main[1].completion_timeout_ms = Some(1_000);
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("service process")
        );

        let mut jobs_only = valid_spec();
        jobs_only.sidecars[0].kind = ProcessKind::RunToCompletion;
        jobs_only.sidecars[0].readiness_probe = None;
        jobs_only.sidecars[0].completion_timeout_ms = Some(1_000);
        jobs_only.main.truncate(1);
        assert!(
            validate(&jobs_only)
                .unwrap_err()
                .to_string()
                .contains("at least one service")
        );
    }

    #[test]
    fn rejects_unknown_fields_and_old_version() {
        let json = r#"{"version":2,"sidecars":[],"main":[{"name":"api","kind":"service","command":["/bin/app"],"restart":"always"}]}"#;
        let value = STANDARD.encode(json);
        assert!(
            decode(&value)
                .unwrap_err()
                .to_string()
                .contains("unknown field")
        );

        let mut spec = valid_spec();
        spec.version = 1;
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("unsupported declaration version")
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
    fn rejects_startup_budget_beyond_ags_deadline() {
        let mut spec = valid_spec();
        spec.sidecars[0]
            .readiness_probe
            .as_mut()
            .unwrap()
            .startup_timeout_ms = 25_000;
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("exceeds the 25000ms limit")
        );
    }

    #[test]
    fn rejects_runtime_environment_users_and_control_probe() {
        let mut spec = valid_spec();
        spec.sidecars[0]
            .env
            .insert(SPEC_ENVIRONMENT.into(), "replacement".into());
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("cannot override")
        );

        let mut spec = valid_spec();
        spec.sidecars[0].readiness_probe.as_mut().unwrap().port = CONTROL_PORT;
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("reserved by campd")
        );

        let mut spec = valid_spec();
        spec.sidecars[0].user = Some(NamedUser {
            name: "../app".into(),
        });
        assert!(
            validate(&spec)
                .unwrap_err()
                .to_string()
                .contains("user name is invalid")
        );

        let mut spec = valid_spec();
        spec.main[1].user = Some(NumericUser {
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
