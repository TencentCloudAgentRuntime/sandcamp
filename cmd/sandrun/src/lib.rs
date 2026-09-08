use std::error::Error;
use std::ffi::{OsStr, OsString};
use std::fmt;
use std::io;
use std::path::{Component, Path, PathBuf};

#[cfg(target_os = "linux")]
mod linux;

pub const USAGE: &str = "\
Usage:
  sandrun --rootfs PATH [OPTIONS] -- /absolute/command [args...]

Options:
  --rootfs PATH          Mounted OCI root filesystem
  --overlay-device PATH  Mount an ext4 block device for OverlayFS upper/work
  --overlay-id NAME      Stable writable-layer identity (required with device)
  --workdir PATH         Absolute working directory inside rootfs (default: /)
  --user NAME            User from the Sidecar rootfs /etc/passwd
  --uid UID              Explicit numeric user ID (requires --gid)
  --gid GID              Explicit numeric primary group ID (requires --uid)
  --standard-mounts      Add proc, dev, cgroup, read-only sys/DNS, tmpfs /tmp and /run
  --disk-mount TARGET    Bind a private overlay-device directory at TARGET
  --bind SOURCE TARGET   Bind a host path read-write; create missing paths
  --ro-bind SOURCE TARGET
                         Bind a host path read-only; create missing paths
  --tmpfs TARGET         Mount a mode=0755 tmpfs at an existing directory
  -h, --help             Print this help
  -V, --version          Print version

sandrun creates only a mount namespace. Network, PID, process group, cgroup,
environment, file descriptors, and capabilities are inherited. The mount
namespace and root switch provide dependency correctness, not a security boundary.
Missing bind sources are created as directories; targets match their sources.
";

const STANDARD_TARGETS: [&str; 7] = [
    "/proc",
    "/dev",
    "/sys",
    "/tmp",
    "/run",
    "/etc/resolv.conf",
    "/etc/hosts",
];

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Bind {
    pub source: PathBuf,
    pub target: PathBuf,
    pub readonly: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ProcessUser {
    Named(String),
    Numeric { uid: u32, gid: u32 },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Config {
    pub rootfs: PathBuf,
    pub overlay_device: Option<PathBuf>,
    pub overlay_id: Option<OsString>,
    pub workdir: PathBuf,
    pub user: Option<ProcessUser>,
    pub standard_mounts: bool,
    pub disk_mounts: Vec<PathBuf>,
    pub binds: Vec<Bind>,
    pub tmpfs: Vec<PathBuf>,
    pub command: Vec<OsString>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ConfigError {
    MissingOption(&'static str),
    MissingValue(&'static str),
    MissingSeparator,
    MissingCommand,
    UnknownOption(OsString),
    InvalidOverlayId,
    InvalidUserName(OsString),
    InvalidUserId {
        option: &'static str,
        value: OsString,
    },
    DuplicateUserOption(&'static str),
    ConflictingUserOptions,
    IncompleteNumericUser,
    DiskMountRequiresOverlayDevice,
    InvalidPath {
        field: &'static str,
        path: PathBuf,
    },
    DuplicateMountTarget(PathBuf),
    OverlappingMountTargets {
        first: PathBuf,
        second: PathBuf,
    },
}

impl fmt::Display for ConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::MissingOption(option) => write!(formatter, "missing required option {option}"),
            Self::MissingValue(option) => write!(formatter, "missing value for {option}"),
            Self::MissingSeparator => formatter.write_str("missing -- before command"),
            Self::MissingCommand => formatter.write_str("missing command after --"),
            Self::UnknownOption(option) => {
                write!(formatter, "unknown option {}", option.to_string_lossy())
            }
            Self::InvalidOverlayId => {
                formatter.write_str("overlay id must contain between 1 and 256 bytes")
            }
            Self::InvalidUserName(name) => {
                write!(
                    formatter,
                    "user name must match [A-Za-z_][A-Za-z0-9_.-]{{0,63}}: {}",
                    name.to_string_lossy()
                )
            }
            Self::InvalidUserId { option, value } => write!(
                formatter,
                "{option} must be a decimal integer between 0 and {}: {}",
                u32::MAX - 1,
                value.to_string_lossy()
            ),
            Self::DuplicateUserOption(option) => {
                write!(formatter, "user option {option} is duplicated")
            }
            Self::ConflictingUserOptions => {
                formatter.write_str("--user cannot be combined with --uid or --gid")
            }
            Self::IncompleteNumericUser => {
                formatter.write_str("--uid and --gid must be specified together")
            }
            Self::DiskMountRequiresOverlayDevice => {
                formatter.write_str("--disk-mount requires --overlay-device")
            }
            Self::InvalidPath { field, path } => {
                write!(
                    formatter,
                    "{field} must be a clean absolute path: {}",
                    path.display()
                )
            }
            Self::DuplicateMountTarget(path) => {
                write!(formatter, "duplicate mount target {}", path.display())
            }
            Self::OverlappingMountTargets { first, second } => write!(
                formatter,
                "overlapping mount targets {} and {} are not supported",
                first.display(),
                second.display()
            ),
        }
    }
}

impl Error for ConfigError {}

#[derive(Debug)]
pub enum RuntimeError {
    UnsupportedPlatform,
    InvalidRootfs(PathBuf),
    InvalidMountTarget {
        target: PathBuf,
        reason: &'static str,
    },
    IncompatibleBind {
        source: PathBuf,
        target: PathBuf,
    },
    UserNotFound(String),
    InvalidNumericUser {
        uid: u32,
        gid: u32,
    },
    InvalidPasswdEntry {
        line: usize,
        reason: &'static str,
    },
    Operation {
        operation: &'static str,
        path: Option<PathBuf>,
        source: io::Error,
    },
}

impl RuntimeError {
    #[cfg(target_os = "linux")]
    pub(crate) fn operation(
        operation: &'static str,
        path: impl Into<Option<PathBuf>>,
        source: io::Error,
    ) -> Self {
        Self::Operation {
            operation,
            path: path.into(),
            source,
        }
    }
}

impl fmt::Display for RuntimeError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::UnsupportedPlatform => formatter.write_str("unsupported_platform"),
            Self::InvalidRootfs(path) => {
                write!(formatter, "invalid_rootfs: {}", path.display())
            }
            Self::InvalidMountTarget { target, reason } => {
                write!(
                    formatter,
                    "invalid_mount_target: {}: {reason}",
                    target.display()
                )
            }
            Self::IncompatibleBind { source, target } => write!(
                formatter,
                "incompatible_bind: {} -> {}",
                source.display(),
                target.display()
            ),
            Self::UserNotFound(user) => write!(formatter, "user_not_found: {user}"),
            Self::InvalidNumericUser { uid, gid } => {
                write!(formatter, "invalid_numeric_user: {uid}:{gid}")
            }
            Self::InvalidPasswdEntry { line, reason } => {
                write!(formatter, "invalid_passwd_entry: line {line}: {reason}")
            }
            Self::Operation {
                operation,
                path,
                source,
            } => {
                write!(formatter, "{operation}")?;
                if let Some(path) = path {
                    write!(formatter, ": {}", path.display())?;
                }
                write!(formatter, ": {source}")
            }
        }
    }
}

impl Error for RuntimeError {
    fn source(&self) -> Option<&(dyn Error + 'static)> {
        match self {
            Self::Operation { source, .. } => Some(source),
            _ => None,
        }
    }
}

pub fn parse_args(arguments: impl IntoIterator<Item = OsString>) -> Result<Config, ConfigError> {
    let arguments = arguments.into_iter().collect::<Vec<_>>();
    let mut index = 0;
    let mut rootfs = None;
    let mut overlay_device = None;
    let mut overlay_id = None;
    let mut workdir = PathBuf::from("/");
    let mut user_name = None;
    let mut uid = None;
    let mut gid = None;
    let mut standard_mounts = false;
    let mut disk_mounts = Vec::new();
    let mut binds = Vec::new();
    let mut tmpfs = Vec::new();
    let mut separator = None;

    while index < arguments.len() {
        let argument = &arguments[index];
        if argument == "--" {
            separator = Some(index);
            break;
        }
        match argument.to_str() {
            Some("--rootfs") => {
                rootfs = Some(path_value(&arguments, &mut index, "--rootfs")?);
            }
            Some("--overlay-device") => {
                overlay_device = Some(path_value(&arguments, &mut index, "--overlay-device")?);
            }
            Some("--overlay-id") => {
                index += 1;
                overlay_id = Some(
                    arguments
                        .get(index)
                        .cloned()
                        .ok_or(ConfigError::MissingValue("--overlay-id"))?,
                );
            }
            Some("--workdir") => {
                workdir = path_value(&arguments, &mut index, "--workdir")?;
            }
            Some("--user") => {
                index += 1;
                let value = arguments
                    .get(index)
                    .cloned()
                    .ok_or(ConfigError::MissingValue("--user"))?;
                if !valid_user_name(&value) {
                    return Err(ConfigError::InvalidUserName(value));
                }
                if user_name
                    .replace(
                        arguments[index]
                            .to_str()
                            .expect("validated user name is UTF-8")
                            .to_owned(),
                    )
                    .is_some()
                {
                    return Err(ConfigError::DuplicateUserOption("--user"));
                }
            }
            Some("--uid") | Some("--gid") => {
                let option = if argument == "--uid" {
                    "--uid"
                } else {
                    "--gid"
                };
                index += 1;
                let value = arguments
                    .get(index)
                    .cloned()
                    .ok_or(ConfigError::MissingValue(option))?;
                let id = parse_user_id(option, &value)?;
                let target = if option == "--uid" {
                    &mut uid
                } else {
                    &mut gid
                };
                if target.replace(id).is_some() {
                    return Err(ConfigError::DuplicateUserOption(option));
                }
            }
            Some("--standard-mounts") => {
                standard_mounts = true;
            }
            Some("--disk-mount") => {
                disk_mounts.push(path_value(&arguments, &mut index, "--disk-mount")?);
            }
            Some("--bind") | Some("--ro-bind") => {
                let option = if argument == "--bind" {
                    "--bind"
                } else {
                    "--ro-bind"
                };
                let source = path_value(&arguments, &mut index, option)?;
                let target = path_value(&arguments, &mut index, option)?;
                binds.push(Bind {
                    source,
                    target,
                    readonly: option == "--ro-bind",
                });
            }
            Some("--tmpfs") => {
                tmpfs.push(path_value(&arguments, &mut index, "--tmpfs")?);
            }
            _ => return Err(ConfigError::UnknownOption(argument.clone())),
        }
        index += 1;
    }

    let separator = separator.ok_or(ConfigError::MissingSeparator)?;
    let command = arguments[separator + 1..].to_vec();
    if command.is_empty() {
        return Err(ConfigError::MissingCommand);
    }

    let rootfs = rootfs.ok_or(ConfigError::MissingOption("--rootfs"))?;
    validate_outer_path("rootfs", &rootfs)?;
    if let Some(device) = &overlay_device {
        validate_outer_path("overlay device", device)?;
    }
    if let Some(id) = &overlay_id
        && (id.as_encoded_bytes().is_empty() || id.as_encoded_bytes().len() > 256)
    {
        return Err(ConfigError::InvalidOverlayId);
    }
    if overlay_device.is_some() && overlay_id.is_none() {
        return Err(ConfigError::MissingOption("--overlay-id"));
    }
    if !disk_mounts.is_empty() && overlay_device.is_none() {
        return Err(ConfigError::DiskMountRequiresOverlayDevice);
    }
    validate_inner_path("workdir", &workdir, true)?;
    validate_inner_path("command", Path::new(&command[0]), false)?;
    for target in &disk_mounts {
        validate_inner_path("disk mount target", target, false)?;
    }
    for bind in &binds {
        validate_outer_path("bind source", &bind.source)?;
        validate_inner_path("bind target", &bind.target, false)?;
    }
    for target in &tmpfs {
        validate_inner_path("tmpfs target", target, false)?;
    }
    validate_mount_targets(standard_mounts, &disk_mounts, &binds, &tmpfs)?;

    let user = match (user_name, uid, gid) {
        (Some(name), None, None) => Some(ProcessUser::Named(name)),
        (Some(_), _, _) => return Err(ConfigError::ConflictingUserOptions),
        (None, Some(uid), Some(gid)) => Some(ProcessUser::Numeric { uid, gid }),
        (None, None, None) => None,
        (None, _, _) => return Err(ConfigError::IncompleteNumericUser),
    };

    Ok(Config {
        rootfs,
        overlay_device,
        overlay_id,
        workdir,
        user,
        standard_mounts,
        disk_mounts,
        binds,
        tmpfs,
        command,
    })
}

fn parse_user_id(option: &'static str, value: &OsStr) -> Result<u32, ConfigError> {
    let parsed = value
        .to_str()
        .filter(|value| !value.is_empty() && value.bytes().all(|byte| byte.is_ascii_digit()))
        .and_then(|value| value.parse::<u32>().ok())
        .filter(|value| *value != u32::MAX);
    parsed.ok_or_else(|| ConfigError::InvalidUserId {
        option,
        value: value.to_os_string(),
    })
}

fn valid_user_name(value: &OsStr) -> bool {
    let Some(value) = value.to_str() else {
        return false;
    };
    let bytes = value.as_bytes();
    if bytes.is_empty() || bytes.len() > 64 {
        return false;
    }
    let first = bytes[0];
    if !first.is_ascii_alphabetic() && first != b'_' {
        return false;
    }
    bytes[1..]
        .iter()
        .all(|value| value.is_ascii_alphanumeric() || matches!(value, b'_' | b'.' | b'-'))
}

fn path_value(
    arguments: &[OsString],
    index: &mut usize,
    option: &'static str,
) -> Result<PathBuf, ConfigError> {
    *index += 1;
    arguments
        .get(*index)
        .map(PathBuf::from)
        .ok_or(ConfigError::MissingValue(option))
}

fn validate_outer_path(field: &'static str, path: &Path) -> Result<(), ConfigError> {
    if clean_absolute(path, false) {
        Ok(())
    } else {
        Err(ConfigError::InvalidPath {
            field,
            path: path.to_path_buf(),
        })
    }
}

fn validate_inner_path(
    field: &'static str,
    path: &Path,
    allow_root: bool,
) -> Result<(), ConfigError> {
    if clean_absolute(path, allow_root) {
        Ok(())
    } else {
        Err(ConfigError::InvalidPath {
            field,
            path: path.to_path_buf(),
        })
    }
}

fn clean_absolute(path: &Path, allow_root: bool) -> bool {
    if !path.is_absolute() || (!allow_root && path == Path::new("/")) {
        return false;
    }
    path.components()
        .all(|component| matches!(component, Component::RootDir | Component::Normal(_)))
}

fn validate_mount_targets(
    standard_mounts: bool,
    disk_mounts: &[PathBuf],
    binds: &[Bind],
    tmpfs: &[PathBuf],
) -> Result<(), ConfigError> {
    let mut targets = Vec::with_capacity(
        disk_mounts.len()
            + binds.len()
            + tmpfs.len()
            + usize::from(standard_mounts) * STANDARD_TARGETS.len(),
    );
    if standard_mounts {
        targets.extend(STANDARD_TARGETS.into_iter().map(PathBuf::from));
    }
    targets.extend(disk_mounts.iter().cloned());
    targets.extend(binds.iter().map(|bind| bind.target.clone()));
    targets.extend(tmpfs.iter().cloned());
    validate_target_paths(&targets)
}

pub(crate) fn validate_target_paths(targets: &[PathBuf]) -> Result<(), ConfigError> {
    for (index, target) in targets.iter().enumerate() {
        for previous in &targets[..index] {
            if target == previous {
                return Err(ConfigError::DuplicateMountTarget(target.clone()));
            }
            if target.starts_with(previous) || previous.starts_with(target) {
                return Err(ConfigError::OverlappingMountTargets {
                    first: previous.clone(),
                    second: target.clone(),
                });
            }
        }
    }
    Ok(())
}

#[cfg(target_os = "linux")]
pub fn run(config: Config) -> Result<(), RuntimeError> {
    linux::run(config)
}

#[cfg(not(target_os = "linux"))]
pub fn run(_config: Config) -> Result<(), RuntimeError> {
    Err(RuntimeError::UnsupportedPlatform)
}

pub fn is_help(argument: &OsStr) -> bool {
    argument == "-h" || argument == "--help"
}

pub fn is_version(argument: &OsStr) -> bool {
    argument == "-V" || argument == "--version"
}

#[cfg(test)]
mod tests {
    use super::*;

    fn arguments(values: &[&str]) -> Vec<OsString> {
        values.iter().map(OsString::from).collect()
    }

    #[test]
    fn parses_minimal_invocation() {
        assert_eq!(
            parse_args(arguments(&[
                "--rootfs",
                "/images/app",
                "--",
                "/bin/app",
                "one"
            ])),
            Ok(Config {
                rootfs: PathBuf::from("/images/app"),
                overlay_device: None,
                overlay_id: None,
                workdir: PathBuf::from("/"),
                user: None,
                standard_mounts: false,
                disk_mounts: Vec::new(),
                binds: Vec::new(),
                tmpfs: Vec::new(),
                command: arguments(&["/bin/app", "one"]),
            })
        );
    }

    #[test]
    fn parses_explicit_mounts_without_shell_encoding() {
        let config = parse_args(arguments(&[
            "--rootfs",
            "/images/app",
            "--overlay-device",
            "/dev/vda",
            "--overlay-id",
            "fastapi",
            "--workdir",
            "/work",
            "--user",
            "app",
            "--standard-mounts",
            "--disk-mount",
            "/var/lib/docker",
            "--bind",
            "/state/app",
            "/var/lib/app",
            "--ro-bind",
            "/config/app.json",
            "/etc/app.json",
            "--tmpfs",
            "/cache",
            "--",
            "/bin/app",
            "argument with spaces",
        ]))
        .unwrap();
        assert_eq!(config.overlay_device, Some(PathBuf::from("/dev/vda")));
        assert_eq!(config.overlay_id, Some(OsString::from("fastapi")));
        assert!(config.standard_mounts);
        assert_eq!(config.disk_mounts, vec![PathBuf::from("/var/lib/docker")]);
        assert_eq!(config.workdir, Path::new("/work"));
        assert_eq!(
            config.user.as_ref(),
            Some(&ProcessUser::Named("app".into()))
        );
        assert_eq!(
            config.binds,
            vec![
                Bind {
                    source: PathBuf::from("/state/app"),
                    target: PathBuf::from("/var/lib/app"),
                    readonly: false,
                },
                Bind {
                    source: PathBuf::from("/config/app.json"),
                    target: PathBuf::from("/etc/app.json"),
                    readonly: true,
                },
            ]
        );
        assert_eq!(config.tmpfs, vec![PathBuf::from("/cache")]);
        assert_eq!(
            config.command,
            arguments(&["/bin/app", "argument with spaces"])
        );
    }

    #[test]
    fn rejects_relative_parent_and_root_targets() {
        for values in [
            &["--rootfs", "relative", "--", "/bin/app"][..],
            &[
                "--rootfs",
                "/images/app",
                "--workdir",
                "/a/../b",
                "--",
                "/bin/app",
            ][..],
            &["--rootfs", "/images/app", "--", "bin/app"][..],
            &["--rootfs", "/images/app", "--tmpfs", "/", "--", "/bin/app"][..],
        ] {
            assert!(matches!(
                parse_args(arguments(values)),
                Err(ConfigError::InvalidPath { .. })
            ));
        }
    }

    #[test]
    fn rejects_duplicate_and_nested_mounts() {
        assert!(matches!(
            parse_args(arguments(&[
                "--rootfs",
                "/images/app",
                "--bind",
                "/one",
                "/state",
                "--tmpfs",
                "/state",
                "--",
                "/bin/app",
            ])),
            Err(ConfigError::DuplicateMountTarget(path)) if path == Path::new("/state")
        ));
        assert!(matches!(
            parse_args(arguments(&[
                "--rootfs",
                "/images/app",
                "--standard-mounts",
                "--tmpfs",
                "/run/app",
                "--",
                "/bin/app",
            ])),
            Err(ConfigError::OverlappingMountTargets { .. })
        ));
    }

    #[test]
    fn requires_rootfs_separator_and_command() {
        assert_eq!(
            parse_args(arguments(&["--", "/bin/app"])),
            Err(ConfigError::MissingOption("--rootfs"))
        );
        assert_eq!(
            parse_args(arguments(&["--rootfs", "/images/app"])),
            Err(ConfigError::MissingSeparator)
        );
        assert_eq!(
            parse_args(arguments(&["--rootfs", "/images/app", "--"])),
            Err(ConfigError::MissingCommand)
        );
        assert_eq!(
            parse_args(arguments(&[
                "--rootfs",
                "/images/app",
                "--overlay-device",
                "/dev/vda",
                "--",
                "/bin/app",
            ])),
            Err(ConfigError::MissingOption("--overlay-id"))
        );
        assert_eq!(
            parse_args(arguments(&[
                "--rootfs",
                "/images/app",
                "--disk-mount",
                "/var/lib/docker",
                "--",
                "/bin/app",
            ])),
            Err(ConfigError::DiskMountRequiresOverlayDevice)
        );
    }

    #[test]
    fn validates_named_users() {
        for name in ["app", "_service", "www-data", "App.User"] {
            let config = parse_args(arguments(&[
                "--rootfs",
                "/images/app",
                "--user",
                name,
                "--",
                "/bin/app",
            ]))
            .unwrap();
            assert_eq!(config.user.as_ref(), Some(&ProcessUser::Named(name.into())));
        }
        for name in ["", "1234", "../app", "app:group", "app user"] {
            assert!(matches!(
                parse_args(arguments(&[
                    "--rootfs",
                    "/images/app",
                    "--user",
                    name,
                    "--",
                    "/bin/app",
                ])),
                Err(ConfigError::InvalidUserName(_))
            ));
        }
    }

    #[test]
    fn parses_and_validates_numeric_users() {
        let config = parse_args(arguments(&[
            "--rootfs",
            "/images/app",
            "--uid",
            "65532",
            "--gid",
            "65531",
            "--",
            "/bin/app",
        ]))
        .unwrap();
        assert_eq!(
            config.user,
            Some(ProcessUser::Numeric {
                uid: 65_532,
                gid: 65_531,
            })
        );

        for value in ["", "-1", "+1", "app", "4294967295", "4294967296"] {
            assert!(matches!(
                parse_args(arguments(&[
                    "--rootfs",
                    "/images/app",
                    "--uid",
                    value,
                    "--gid",
                    "1",
                    "--",
                    "/bin/app",
                ])),
                Err(ConfigError::InvalidUserId {
                    option: "--uid",
                    ..
                })
            ));
        }
    }

    #[test]
    fn rejects_incomplete_conflicting_and_duplicate_users() {
        assert_eq!(
            parse_args(arguments(&[
                "--rootfs",
                "/images/app",
                "--uid",
                "1000",
                "--",
                "/bin/app",
            ])),
            Err(ConfigError::IncompleteNumericUser)
        );
        assert_eq!(
            parse_args(arguments(&[
                "--rootfs",
                "/images/app",
                "--user",
                "app",
                "--uid",
                "1000",
                "--gid",
                "1000",
                "--",
                "/bin/app",
            ])),
            Err(ConfigError::ConflictingUserOptions)
        );
        assert_eq!(
            parse_args(arguments(&[
                "--rootfs",
                "/images/app",
                "--user",
                "app",
                "--user",
                "root",
                "--",
                "/bin/app",
            ])),
            Err(ConfigError::DuplicateUserOption("--user"))
        );
    }
}
