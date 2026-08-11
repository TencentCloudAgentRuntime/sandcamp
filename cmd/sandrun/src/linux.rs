use crate::{Bind, Config, ProcessUser, RuntimeError, validate_target_paths};
use sha2::{Digest, Sha256};
use std::ffi::{CString, OsStr, OsString};
use std::fs::{self, DirBuilder, File, OpenOptions};
use std::io::{self, Read, Write};
use std::os::fd::AsRawFd;
use std::os::unix::ffi::{OsStrExt, OsStringExt};
use std::os::unix::fs::{DirBuilderExt, FileTypeExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::ptr;
use std::time::{SystemTime, UNIX_EPOCH};

const OVERLAY_BASE: &str = "/var/lib/sandcamp/overlay";
const OVERLAY_DEVICE_MOUNT: &str = "/var/lib/sandcamp/overlay-device";
const OVERLAY_DEVICE_NAMESPACE: &str = "sandcamp/overlay/v1";
const MAX_PASSWD_BYTES: u64 = 1024 * 1024;
const LINUX_CAPABILITY_VERSION_3: u32 = 0x2008_0522;

struct PreparedBind {
    source: PathBuf,
    target: PathBuf,
    readonly: bool,
    recursive: bool,
}

enum MountOperation {
    Proc(PathBuf),
    Bind(PreparedBind),
    Tmpfs {
        target: PathBuf,
        mode: u32,
    },
    DnsFiles {
        target: PathBuf,
        resolv_conf: PathBuf,
        hosts: PathBuf,
    },
}

struct OverlayRootfs {
    merged: PathBuf,
    _lock: Option<File>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct UserIdentity {
    uid: u32,
    gid: u32,
}

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

pub(crate) fn run(config: Config) -> Result<(), RuntimeError> {
    let lower = prepare_rootfs(&config.rootfs)?;
    let identity = match &config.user {
        Some(ProcessUser::Named(user)) => Some(resolve_user(&lower, user)?),
        Some(ProcessUser::Numeric { uid, gid }) => {
            if *uid == u32::MAX || *gid == u32::MAX {
                return Err(RuntimeError::InvalidNumericUser {
                    uid: *uid,
                    gid: *gid,
                });
            }
            Some(UserIdentity {
                uid: *uid,
                gid: *gid,
            })
        }
        None => None,
    };

    unshare_mount_namespace()?;
    make_mounts_private()?;
    let overlay_base = prepare_overlay_base(config.overlay_device.as_deref())?;
    let overlay = mount_overlay_rootfs(&lower, &overlay_base, config.overlay_id.as_deref())?;
    let rootfs = &overlay.merged;

    prepare_workdir(rootfs, &config.workdir)?;
    prepare_executable(rootfs, Path::new(&config.command[0]))?;
    validate_resolved_mount_targets(rootfs, &config)?;
    let operations = prepare_mounts(rootfs, &config)?;
    for operation in operations {
        apply_mount(operation)?;
    }
    enter_rootfs(rootfs)?;
    change_directory(&config.workdir)?;
    if let Some(identity) = identity {
        apply_identity(identity)?;
    }

    let mut command = Command::new(&config.command[0]);
    command.args(&config.command[1..]);
    let error = command.exec();
    Err(RuntimeError::operation(
        "exec_failed",
        Some(PathBuf::from(&config.command[0])),
        error,
    ))
}

fn prepare_rootfs(path: &Path) -> Result<PathBuf, RuntimeError> {
    let rootfs = fs::canonicalize(path).map_err(|error| {
        RuntimeError::operation("rootfs_resolve_failed", path.to_path_buf(), error)
    })?;
    let metadata = fs::metadata(&rootfs)
        .map_err(|error| RuntimeError::operation("rootfs_stat_failed", rootfs.clone(), error))?;
    if rootfs == Path::new("/") || !metadata.is_dir() {
        return Err(RuntimeError::InvalidRootfs(rootfs));
    }
    Ok(rootfs)
}

fn resolve_user(rootfs: &Path, user: &str) -> Result<UserIdentity, RuntimeError> {
    let passwd = resolve_target(rootfs, Path::new("/etc/passwd"))?;
    let metadata = fs::metadata(&passwd)
        .map_err(|error| RuntimeError::operation("passwd_stat_failed", passwd.clone(), error))?;
    if !metadata.is_file() {
        return Err(RuntimeError::InvalidPasswdEntry {
            line: 0,
            reason: "/etc/passwd is not a regular file",
        });
    }
    if metadata.len() > MAX_PASSWD_BYTES {
        return Err(RuntimeError::operation(
            "passwd_too_large",
            passwd,
            io::Error::from_raw_os_error(libc::EFBIG),
        ));
    }
    let mut contents = Vec::with_capacity(usize::try_from(metadata.len()).unwrap_or(0));
    File::open(&passwd)
        .and_then(|file| file.take(MAX_PASSWD_BYTES + 1).read_to_end(&mut contents))
        .map_err(|error| RuntimeError::operation("passwd_read_failed", passwd.clone(), error))?;
    if contents.len() as u64 > MAX_PASSWD_BYTES {
        return Err(RuntimeError::operation(
            "passwd_too_large",
            passwd,
            io::Error::from_raw_os_error(libc::EFBIG),
        ));
    }
    let contents =
        std::str::from_utf8(&contents).map_err(|_| RuntimeError::InvalidPasswdEntry {
            line: 0,
            reason: "/etc/passwd is not UTF-8",
        })?;
    parse_passwd(contents, user)
}

fn parse_passwd(contents: &str, user: &str) -> Result<UserIdentity, RuntimeError> {
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
            return Err(RuntimeError::InvalidPasswdEntry {
                line: line_number,
                reason: "user name is duplicated",
            });
        }
        if fields.len() != 7 {
            return Err(RuntimeError::InvalidPasswdEntry {
                line: line_number,
                reason: "matching entry must contain seven fields",
            });
        }
        let uid = parse_passwd_id(fields[2], line_number, "UID is invalid")?;
        let gid = parse_passwd_id(fields[3], line_number, "GID is invalid")?;
        found = Some(UserIdentity { uid, gid });
    }
    found.ok_or_else(|| RuntimeError::UserNotFound(user.to_owned()))
}

fn parse_passwd_id(value: &str, line: usize, reason: &'static str) -> Result<u32, RuntimeError> {
    let id = value
        .parse::<u32>()
        .map_err(|_| RuntimeError::InvalidPasswdEntry { line, reason })?;
    if id == u32::MAX {
        return Err(RuntimeError::InvalidPasswdEntry { line, reason });
    }
    Ok(id)
}

fn apply_identity(identity: UserIdentity) -> Result<(), RuntimeError> {
    let non_root = identity.uid != 0;
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
        return Err(RuntimeError::operation(
            "ambient_capabilities_clear_failed",
            None,
            io::Error::last_os_error(),
        ));
    }
    if unsafe { libc::setgroups(0, ptr::null()) } != 0 {
        return Err(RuntimeError::operation(
            "supplementary_groups_clear_failed",
            None,
            io::Error::last_os_error(),
        ));
    }
    if unsafe { libc::setresgid(identity.gid, identity.gid, identity.gid) } != 0 {
        return Err(RuntimeError::operation(
            "setresgid_failed",
            None,
            io::Error::last_os_error(),
        ));
    }
    if unsafe { libc::setresuid(identity.uid, identity.uid, identity.uid) } != 0 {
        return Err(RuntimeError::operation(
            "setresuid_failed",
            None,
            io::Error::last_os_error(),
        ));
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
            return Err(RuntimeError::operation(
                "capabilities_clear_failed",
                None,
                io::Error::last_os_error(),
            ));
        }
        if unsafe { libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) } != 0 {
            return Err(RuntimeError::operation(
                "no_new_privileges_failed",
                None,
                io::Error::last_os_error(),
            ));
        }
    }
    if unsafe { libc::getuid() } != identity.uid
        || unsafe { libc::geteuid() } != identity.uid
        || unsafe { libc::getgid() } != identity.gid
        || unsafe { libc::getegid() } != identity.gid
    {
        return Err(RuntimeError::operation(
            "identity_verification_failed",
            None,
            io::Error::from_raw_os_error(libc::EPERM),
        ));
    }
    Ok(())
}

fn prepare_overlay_base(device: Option<&Path>) -> Result<PathBuf, RuntimeError> {
    let Some(device) = device else {
        return Ok(PathBuf::from(OVERLAY_BASE));
    };
    let device = fs::canonicalize(device).map_err(|error| {
        RuntimeError::operation("overlay_device_resolve_failed", device.to_path_buf(), error)
    })?;
    let metadata = fs::metadata(&device).map_err(|error| {
        RuntimeError::operation("overlay_device_stat_failed", device.clone(), error)
    })?;
    if !metadata.file_type().is_block_device() {
        return Err(RuntimeError::InvalidMountTarget {
            target: device,
            reason: "overlay device is not a block device",
        });
    }

    let mountpoint = Path::new(OVERLAY_DEVICE_MOUNT);
    ensure_private_directory(mountpoint, "overlay_device_mountpoint_create_failed")?;
    mount_raw(
        Some(device.as_os_str()),
        mountpoint,
        Some(OsStr::new("ext4")),
        0,
        None,
        "overlay_device_mount_failed",
    )?;
    let base = mountpoint.join(OVERLAY_DEVICE_NAMESPACE);
    ensure_private_directory(&base, "overlay_base_create_failed")?;
    Ok(base)
}

fn mount_overlay_rootfs(
    lower: &Path,
    base: &Path,
    overlay_id: Option<&OsStr>,
) -> Result<OverlayRootfs, RuntimeError> {
    if base.starts_with(lower) || lower.starts_with(base) {
        return Err(RuntimeError::InvalidRootfs(lower.to_path_buf()));
    }

    ensure_private_directory(base, "overlay_base_create_failed")?;
    let (upper, work, merged, lock) = if let Some(overlay_id) = overlay_id {
        stable_overlay_paths(base, lower, overlay_id)?
    } else {
        let workspace = create_overlay_workspace(base)?;
        let upper = workspace.join("upper");
        let work = workspace.join("work");
        let merged = workspace.join("merged");
        for directory in [&work, &merged] {
            ensure_private_directory(directory, "overlay_directory_create_failed")?;
        }
        (upper, work, merged, None)
    };

    ensure_overlay_upper_directory(&upper, lower)?;
    let options = OsStringBuffer::overlay(lower, &upper, &work);
    mount_raw(
        Some(OsStr::new("overlay")),
        &merged,
        Some(OsStr::new("overlay")),
        0,
        Some(options.as_os_str()),
        "overlay_mount_failed",
    )?;
    Ok(OverlayRootfs {
        merged,
        _lock: lock,
    })
}

fn stable_overlay_paths(
    base: &Path,
    lower: &Path,
    overlay_id: &OsStr,
) -> Result<(PathBuf, PathBuf, PathBuf, Option<File>), RuntimeError> {
    let identity = overlay_identity(overlay_id, lower);
    let digest = Sha256::digest(&identity);
    let workspace = base.join(hex_lower(&digest));
    ensure_private_directory(&workspace, "overlay_workspace_create_failed")?;
    let lock = acquire_overlay_lock(&workspace.join("lock"))?;
    ensure_identity(&workspace.join("identity"), &identity)?;

    let upper = workspace.join("upper");
    let work = workspace.join("work");
    let merged_base = workspace.join("merged");
    for directory in [&work, &merged_base] {
        ensure_private_directory(directory, "overlay_directory_create_failed")?;
    }
    let merged = create_overlay_workspace(&merged_base)?;
    Ok((upper, work, merged, Some(lock)))
}

fn overlay_identity(overlay_id: &OsStr, lower: &Path) -> Vec<u8> {
    let mut identity = b"sandrun-overlay-v1".to_vec();
    append_identity_part(&mut identity, overlay_id.as_bytes());
    append_identity_part(&mut identity, lower.as_os_str().as_bytes());
    identity
}

fn append_identity_part(identity: &mut Vec<u8>, value: &[u8]) {
    identity.extend_from_slice(&(value.len() as u64).to_be_bytes());
    identity.extend_from_slice(value);
}

fn hex_lower(value: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut encoded = String::with_capacity(value.len() * 2);
    for byte in value {
        encoded.push(HEX[(byte >> 4) as usize] as char);
        encoded.push(HEX[(byte & 0x0f) as usize] as char);
    }
    encoded
}

fn ensure_identity(path: &Path, expected: &[u8]) -> Result<(), RuntimeError> {
    match OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)
    {
        Ok(mut identity) => identity.write_all(expected).map_err(|error| {
            RuntimeError::operation("overlay_identity_write_failed", path.to_path_buf(), error)
        }),
        Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {
            let actual = fs::read(path).map_err(|error| {
                RuntimeError::operation("overlay_identity_read_failed", path.to_path_buf(), error)
            })?;
            if actual == expected {
                Ok(())
            } else {
                Err(RuntimeError::operation(
                    "overlay_identity_mismatch",
                    path.to_path_buf(),
                    io::Error::new(
                        io::ErrorKind::AlreadyExists,
                        "overlay hash maps to a different identity",
                    ),
                ))
            }
        }
        Err(error) => Err(RuntimeError::operation(
            "overlay_identity_create_failed",
            path.to_path_buf(),
            error,
        )),
    }
}

fn acquire_overlay_lock(path: &Path) -> Result<File, RuntimeError> {
    let lock = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(path)
        .map_err(|error| {
            RuntimeError::operation("overlay_lock_open_failed", path.to_path_buf(), error)
        })?;
    let descriptor = lock.as_raw_fd();
    if unsafe { libc::flock(descriptor, libc::LOCK_EX | libc::LOCK_NB) } != 0 {
        return Err(RuntimeError::operation(
            "overlay_workspace_busy",
            path.to_path_buf(),
            io::Error::last_os_error(),
        ));
    }
    let flags = unsafe { libc::fcntl(descriptor, libc::F_GETFD) };
    if flags < 0
        || unsafe { libc::fcntl(descriptor, libc::F_SETFD, flags & !libc::FD_CLOEXEC) } != 0
    {
        return Err(RuntimeError::operation(
            "overlay_lock_inherit_failed",
            path.to_path_buf(),
            io::Error::last_os_error(),
        ));
    }
    Ok(lock)
}

fn ensure_overlay_upper_directory(path: &Path, lower: &Path) -> Result<(), RuntimeError> {
    let created = match DirBuilder::new().mode(0o700).create(path) {
        Ok(()) => true,
        Err(error) if error.kind() == io::ErrorKind::AlreadyExists => false,
        Err(error) => {
            return Err(RuntimeError::operation(
                "overlay_upper_create_failed",
                path.to_path_buf(),
                error,
            ));
        }
    };
    let metadata = fs::symlink_metadata(path).map_err(|error| {
        RuntimeError::operation("overlay_upper_stat_failed", path.to_path_buf(), error)
    })?;
    if !metadata.is_dir() || metadata.file_type().is_symlink() {
        return Err(RuntimeError::InvalidMountTarget {
            target: path.to_path_buf(),
            reason: "overlay upper is not a real directory",
        });
    }
    if !created {
        return Ok(());
    }

    let lower_metadata = fs::metadata(lower).map_err(|error| {
        RuntimeError::operation("rootfs_stat_failed", lower.to_path_buf(), error)
    })?;
    let encoded = c_string(
        path.as_os_str(),
        "overlay_upper_chown_failed",
        Some(path.to_path_buf()),
    )?;
    if unsafe { libc::chown(encoded.as_ptr(), lower_metadata.uid(), lower_metadata.gid()) } != 0 {
        return Err(RuntimeError::operation(
            "overlay_upper_chown_failed",
            path.to_path_buf(),
            io::Error::last_os_error(),
        ));
    }
    fs::set_permissions(
        path,
        fs::Permissions::from_mode(lower_metadata.mode() & 0o7777),
    )
    .map_err(|error| {
        RuntimeError::operation("overlay_upper_chmod_failed", path.to_path_buf(), error)
    })
}

fn ensure_private_directory(path: &Path, operation: &'static str) -> Result<(), RuntimeError> {
    DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(path)
        .map_err(|error| RuntimeError::operation(operation, path.to_path_buf(), error))?;
    let metadata = fs::symlink_metadata(path)
        .map_err(|error| RuntimeError::operation(operation, path.to_path_buf(), error))?;
    if !metadata.is_dir() || metadata.file_type().is_symlink() {
        return Err(RuntimeError::InvalidMountTarget {
            target: path.to_path_buf(),
            reason: "overlay directory is not a real directory",
        });
    }
    Ok(())
}

fn create_overlay_workspace(base: &Path) -> Result<PathBuf, RuntimeError> {
    let pid = unsafe { libc::getpid() };
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    for attempt in 0_u8..=127 {
        let workspace = base.join(format!("{pid}-{nonce:x}-{attempt}"));
        match DirBuilder::new().mode(0o700).create(&workspace) {
            Ok(()) => return Ok(workspace),
            Err(error) if error.kind() == io::ErrorKind::AlreadyExists => continue,
            Err(error) => {
                return Err(RuntimeError::operation(
                    "overlay_workspace_create_failed",
                    workspace,
                    error,
                ));
            }
        }
    }
    Err(RuntimeError::operation(
        "overlay_workspace_create_failed",
        base.to_path_buf(),
        io::Error::new(
            io::ErrorKind::AlreadyExists,
            "could not allocate a unique overlay workspace",
        ),
    ))
}

fn prepare_workdir(rootfs: &Path, workdir: &Path) -> Result<(), RuntimeError> {
    let resolved = resolve_target(rootfs, workdir)?;
    if !fs::metadata(&resolved)
        .map_err(|error| RuntimeError::operation("workdir_stat_failed", resolved.clone(), error))?
        .is_dir()
    {
        return Err(RuntimeError::InvalidMountTarget {
            target: workdir.to_path_buf(),
            reason: "workdir is not a directory",
        });
    }
    Ok(())
}

fn prepare_executable(rootfs: &Path, executable: &Path) -> Result<(), RuntimeError> {
    let resolved = resolve_target(rootfs, executable)?;
    let metadata = fs::metadata(&resolved).map_err(|error| {
        RuntimeError::operation("executable_stat_failed", resolved.clone(), error)
    })?;
    if !metadata.is_file() {
        return Err(RuntimeError::InvalidMountTarget {
            target: executable.to_path_buf(),
            reason: "command is not a regular file",
        });
    }
    if metadata.permissions().mode() & 0o111 == 0 {
        return Err(RuntimeError::InvalidMountTarget {
            target: executable.to_path_buf(),
            reason: "command is not executable",
        });
    }
    Ok(())
}

fn prepare_mounts(rootfs: &Path, config: &Config) -> Result<Vec<MountOperation>, RuntimeError> {
    let mut operations = Vec::new();
    if config.standard_mounts {
        operations.push(MountOperation::Proc(resolve_directory(rootfs, "/proc")?));
        operations.push(MountOperation::Bind(prepare_recursive_bind(
            rootfs,
            &Bind {
                source: PathBuf::from("/dev"),
                target: PathBuf::from("/dev"),
                readonly: false,
            },
        )?));
        operations.push(MountOperation::Bind(prepare_bind(
            rootfs,
            &Bind {
                source: PathBuf::from("/sys"),
                target: PathBuf::from("/sys"),
                readonly: true,
            },
        )?));
        operations.push(MountOperation::Tmpfs {
            target: resolve_directory(rootfs, "/tmp")?,
            mode: 0o1777,
        });
        operations.push(MountOperation::Tmpfs {
            target: resolve_directory(rootfs, "/run")?,
            mode: 0o755,
        });
        operations.push(MountOperation::DnsFiles {
            target: resolve_directory(rootfs, "/etc")?,
            resolv_conf: prepare_regular_source(Path::new("/etc/resolv.conf"))?,
            hosts: prepare_regular_source(Path::new("/etc/hosts"))?,
        });
    }
    for bind in &config.binds {
        operations.push(MountOperation::Bind(prepare_bind(rootfs, bind)?));
    }
    for target in &config.tmpfs {
        operations.push(MountOperation::Tmpfs {
            target: resolve_directory(rootfs, target)?,
            mode: 0o755,
        });
    }
    Ok(operations)
}

fn validate_resolved_mount_targets(rootfs: &Path, config: &Config) -> Result<(), RuntimeError> {
    let mut targets = Vec::new();
    if config.standard_mounts {
        for target in ["/proc", "/dev", "/sys", "/tmp", "/run"] {
            targets.push(resolve_inner_target(rootfs, Path::new(target))?);
        }
        let etc = resolve_target(rootfs, Path::new("/etc"))?;
        let _ = inner_path(rootfs, &etc, Path::new("/etc"))?;
        targets.push(inner_path(
            rootfs,
            &etc.join("resolv.conf"),
            Path::new("/etc/resolv.conf"),
        )?);
        targets.push(inner_path(
            rootfs,
            &etc.join("hosts"),
            Path::new("/etc/hosts"),
        )?);
    }
    for bind in &config.binds {
        targets.push(resolve_inner_target(rootfs, &bind.target)?);
    }
    for target in &config.tmpfs {
        targets.push(resolve_inner_target(rootfs, target)?);
    }
    validate_target_paths(&targets).map_err(|error| {
        RuntimeError::operation(
            "resolved_mount_targets_invalid",
            None,
            io::Error::new(io::ErrorKind::InvalidInput, error),
        )
    })
}

fn resolve_inner_target(rootfs: &Path, target: &Path) -> Result<PathBuf, RuntimeError> {
    let resolved = resolve_target(rootfs, target)?;
    inner_path(rootfs, &resolved, target)
}

fn inner_path(rootfs: &Path, resolved: &Path, original: &Path) -> Result<PathBuf, RuntimeError> {
    let relative = resolved
        .strip_prefix(rootfs)
        .map_err(|_| RuntimeError::InvalidMountTarget {
            target: original.to_path_buf(),
            reason: "resolved target escapes rootfs",
        })?;
    if relative.as_os_str().is_empty() {
        return Err(RuntimeError::InvalidMountTarget {
            target: original.to_path_buf(),
            reason: "resolved target is the rootfs",
        });
    }
    Ok(Path::new("/").join(relative))
}

fn prepare_bind(rootfs: &Path, bind: &Bind) -> Result<PreparedBind, RuntimeError> {
    prepare_bind_with_recursion(rootfs, bind, false)
}

fn prepare_recursive_bind(rootfs: &Path, bind: &Bind) -> Result<PreparedBind, RuntimeError> {
    prepare_bind_with_recursion(rootfs, bind, true)
}

fn prepare_bind_with_recursion(
    rootfs: &Path,
    bind: &Bind,
    recursive: bool,
) -> Result<PreparedBind, RuntimeError> {
    let source = fs::canonicalize(&bind.source).map_err(|error| {
        RuntimeError::operation("bind_source_resolve_failed", bind.source.clone(), error)
    })?;
    let target = resolve_target(rootfs, &bind.target)?;
    let source_metadata = fs::metadata(&source).map_err(|error| {
        RuntimeError::operation("bind_source_stat_failed", source.clone(), error)
    })?;
    let target_metadata = fs::metadata(&target).map_err(|error| {
        RuntimeError::operation("bind_target_stat_failed", target.clone(), error)
    })?;
    if source_metadata.is_dir() != target_metadata.is_dir()
        || source_metadata.is_file() != target_metadata.is_file()
    {
        return Err(RuntimeError::IncompatibleBind { source, target });
    }
    Ok(PreparedBind {
        source,
        target,
        readonly: bind.readonly,
        recursive: recursive && source_metadata.is_dir(),
    })
}

fn prepare_regular_source(path: &Path) -> Result<PathBuf, RuntimeError> {
    let source = fs::canonicalize(path).map_err(|error| {
        RuntimeError::operation("bind_source_resolve_failed", path.to_path_buf(), error)
    })?;
    let metadata = fs::metadata(&source).map_err(|error| {
        RuntimeError::operation("bind_source_stat_failed", source.clone(), error)
    })?;
    if !metadata.is_file() {
        return Err(RuntimeError::InvalidMountTarget {
            target: path.to_path_buf(),
            reason: "bind source is not a regular file",
        });
    }
    Ok(source)
}

fn resolve_directory(rootfs: &Path, target: impl AsRef<Path>) -> Result<PathBuf, RuntimeError> {
    let target = target.as_ref();
    let resolved = resolve_target(rootfs, target)?;
    if !fs::metadata(&resolved)
        .map_err(|error| {
            RuntimeError::operation("mount_target_stat_failed", resolved.clone(), error)
        })?
        .is_dir()
    {
        return Err(RuntimeError::InvalidMountTarget {
            target: target.to_path_buf(),
            reason: "mount target is not a directory",
        });
    }
    Ok(resolved)
}

fn resolve_target(rootfs: &Path, target: &Path) -> Result<PathBuf, RuntimeError> {
    let mut logical =
        normalize_inner_path(target).ok_or_else(|| RuntimeError::InvalidMountTarget {
            target: target.to_path_buf(),
            reason: "target escapes rootfs",
        })?;
    let mut followed = 0_u8;
    loop {
        let components = logical
            .components()
            .filter_map(|component| match component {
                std::path::Component::Normal(value) => Some(value.to_os_string()),
                _ => None,
            })
            .collect::<Vec<_>>();
        let mut physical = rootfs.to_path_buf();
        let mut inner_parent = PathBuf::from("/");
        let mut redirected = None;
        for (index, component) in components.iter().enumerate() {
            physical.push(component);
            let metadata = fs::symlink_metadata(&physical).map_err(|error| {
                RuntimeError::operation("mount_target_resolve_failed", physical.clone(), error)
            })?;
            if metadata.file_type().is_symlink() {
                followed = followed.saturating_add(1);
                if followed > 40 {
                    return Err(RuntimeError::InvalidMountTarget {
                        target: target.to_path_buf(),
                        reason: "target contains too many symlinks",
                    });
                }
                let link = fs::read_link(&physical).map_err(|error| {
                    RuntimeError::operation("mount_target_readlink_failed", physical.clone(), error)
                })?;
                let mut next = if link.is_absolute() {
                    link
                } else {
                    inner_parent.join(link)
                };
                for remaining in &components[index + 1..] {
                    next.push(remaining);
                }
                redirected = Some(next);
                break;
            }
            inner_parent.push(component);
        }
        let Some(next) = redirected else {
            return Ok(physical);
        };
        logical = normalize_inner_path(&next).ok_or_else(|| RuntimeError::InvalidMountTarget {
            target: target.to_path_buf(),
            reason: "target escapes rootfs through a symlink",
        })?;
    }
}

fn normalize_inner_path(path: &Path) -> Option<PathBuf> {
    if !path.is_absolute() {
        return None;
    }
    let mut components = Vec::<OsString>::new();
    for component in path.components() {
        match component {
            std::path::Component::RootDir | std::path::Component::CurDir => {}
            std::path::Component::Normal(value) => components.push(value.to_os_string()),
            std::path::Component::ParentDir => {
                components.pop()?;
            }
            std::path::Component::Prefix(_) => return None,
        }
    }
    let mut normalized = PathBuf::from("/");
    normalized.extend(components);
    Some(normalized)
}

fn unshare_mount_namespace() -> Result<(), RuntimeError> {
    if unsafe { libc::unshare(libc::CLONE_NEWNS) } == 0 {
        Ok(())
    } else {
        Err(RuntimeError::operation(
            "mount_namespace_unshare_failed",
            None,
            io::Error::last_os_error(),
        ))
    }
}

fn make_mounts_private() -> Result<(), RuntimeError> {
    mount_raw(
        None,
        Path::new("/"),
        None,
        libc::MS_REC | libc::MS_PRIVATE,
        None,
        "mount_propagation_private_failed",
    )
}

fn apply_mount(operation: MountOperation) -> Result<(), RuntimeError> {
    match operation {
        MountOperation::Proc(target) => mount_raw(
            Some(OsStr::new("proc")),
            &target,
            Some(OsStr::new("proc")),
            libc::MS_NOSUID | libc::MS_NODEV | libc::MS_NOEXEC,
            None,
            "proc_mount_failed",
        ),
        MountOperation::Bind(bind) => {
            let mut flags = libc::MS_BIND;
            if bind.recursive {
                flags |= libc::MS_REC;
            }
            mount_raw(
                Some(bind.source.as_os_str()),
                &bind.target,
                None,
                flags,
                None,
                "bind_mount_failed",
            )?;
            if bind.readonly {
                mount_raw(
                    None,
                    &bind.target,
                    None,
                    libc::MS_BIND | libc::MS_REMOUNT | libc::MS_RDONLY,
                    None,
                    "bind_remount_readonly_failed",
                )?;
            }
            Ok(())
        }
        MountOperation::Tmpfs { target, mode } => mount_raw(
            Some(OsStr::new("tmpfs")),
            &target,
            Some(OsStr::new("tmpfs")),
            libc::MS_NOSUID | libc::MS_NODEV,
            Some(OsStringBuffer::mode(mode).as_os_str()),
            "tmpfs_mount_failed",
        ),
        MountOperation::DnsFiles {
            target,
            resolv_conf,
            hosts,
        } => {
            let resolv_target = target.join("resolv.conf");
            let hosts_target = target.join("hosts");
            ensure_regular_mount_target(&resolv_target)?;
            ensure_regular_mount_target(&hosts_target)?;
            bind_file_readonly(&resolv_conf, &resolv_target)?;
            bind_file_readonly(&hosts, &hosts_target)
        }
    }
}

fn ensure_regular_mount_target(target: &Path) -> Result<(), RuntimeError> {
    match fs::symlink_metadata(target) {
        Ok(metadata) if metadata.is_file() && !metadata.file_type().is_symlink() => return Ok(()),
        Ok(metadata) if metadata.file_type().is_symlink() => {
            fs::remove_file(target).map_err(|error| {
                RuntimeError::operation("dns_target_replace_failed", target.to_path_buf(), error)
            })?;
        }
        Ok(_) => {
            return Err(RuntimeError::InvalidMountTarget {
                target: target.to_path_buf(),
                reason: "DNS mount target is not a regular file",
            });
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => {
            return Err(RuntimeError::operation(
                "dns_target_stat_failed",
                target.to_path_buf(),
                error,
            ));
        }
    }
    OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o644)
        .open(target)
        .map(|_| ())
        .map_err(|error| {
            RuntimeError::operation("dns_target_create_failed", target.to_path_buf(), error)
        })
}

fn bind_file_readonly(source: &Path, target: &Path) -> Result<(), RuntimeError> {
    mount_raw(
        Some(source.as_os_str()),
        target,
        None,
        libc::MS_BIND,
        None,
        "bind_mount_failed",
    )?;
    mount_raw(
        None,
        target,
        None,
        libc::MS_BIND | libc::MS_REMOUNT | libc::MS_RDONLY,
        None,
        "bind_remount_readonly_failed",
    )
}

struct OsStringBuffer(OsString);

impl OsStringBuffer {
    fn mode(mode: u32) -> Self {
        Self(OsString::from(format!("mode={mode:o}")))
    }

    fn overlay(lower: &Path, upper: &Path, work: &Path) -> Self {
        let mut bytes = b"lowerdir=".to_vec();
        append_overlay_path(&mut bytes, lower);
        bytes.extend_from_slice(b",upperdir=");
        append_overlay_path(&mut bytes, upper);
        bytes.extend_from_slice(b",workdir=");
        append_overlay_path(&mut bytes, work);
        Self(OsString::from_vec(bytes))
    }

    fn as_os_str(&self) -> &OsStr {
        &self.0
    }
}

fn append_overlay_path(buffer: &mut Vec<u8>, path: &Path) {
    for byte in path.as_os_str().as_bytes() {
        if matches!(*byte, b'\\' | b',' | b':') {
            buffer.push(b'\\');
        }
        buffer.push(*byte);
    }
}

fn enter_rootfs(rootfs: &Path) -> Result<(), RuntimeError> {
    change_directory_outer(rootfs, "rootfs_chdir_failed")?;
    let dot = CString::new(".").expect("static path");
    if unsafe { libc::syscall(libc::SYS_pivot_root, dot.as_ptr(), dot.as_ptr()) } != 0 {
        return Err(RuntimeError::operation(
            "pivot_root_failed",
            Some(rootfs.to_path_buf()),
            io::Error::last_os_error(),
        ));
    }
    if unsafe { libc::umount2(dot.as_ptr(), libc::MNT_DETACH) } != 0 {
        return Err(RuntimeError::operation(
            "old_root_unmount_failed",
            None,
            io::Error::last_os_error(),
        ));
    }
    change_directory_outer(Path::new("/"), "new_root_chdir_failed")
}

fn change_directory(path: &Path) -> Result<(), RuntimeError> {
    change_directory_outer(path, "workdir_chdir_failed")
}

fn change_directory_outer(path: &Path, operation: &'static str) -> Result<(), RuntimeError> {
    let encoded = c_string(path.as_os_str(), operation, Some(path.to_path_buf()))?;
    if unsafe { libc::chdir(encoded.as_ptr()) } == 0 {
        Ok(())
    } else {
        Err(RuntimeError::operation(
            operation,
            Some(path.to_path_buf()),
            io::Error::last_os_error(),
        ))
    }
}

fn mount_raw(
    source: Option<&OsStr>,
    target: &Path,
    filesystem: Option<&OsStr>,
    flags: libc::c_ulong,
    data: Option<&OsStr>,
    operation: &'static str,
) -> Result<(), RuntimeError> {
    let source = source
        .map(|value| c_string(value, operation, None))
        .transpose()?;
    let target_encoded = c_string(target.as_os_str(), operation, Some(target.to_path_buf()))?;
    let filesystem = filesystem
        .map(|value| c_string(value, operation, None))
        .transpose()?;
    let data = data
        .map(|value| c_string(value, operation, None))
        .transpose()?;

    let result = unsafe {
        libc::mount(
            source.as_ref().map_or(ptr::null(), |value| value.as_ptr()),
            target_encoded.as_ptr(),
            filesystem
                .as_ref()
                .map_or(ptr::null(), |value| value.as_ptr()),
            flags,
            data.as_ref()
                .map_or(ptr::null(), |value| value.as_ptr().cast()),
        )
    };
    if result == 0 {
        Ok(())
    } else {
        Err(RuntimeError::operation(
            operation,
            Some(target.to_path_buf()),
            io::Error::last_os_error(),
        ))
    }
}

fn c_string(
    value: &OsStr,
    operation: &'static str,
    path: Option<PathBuf>,
) -> Result<CString, RuntimeError> {
    CString::new(value.as_bytes()).map_err(|_| {
        RuntimeError::operation(
            operation,
            path,
            io::Error::new(io::ErrorKind::InvalidInput, "path contains NUL"),
        )
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn temporary_root(case: &str) -> PathBuf {
        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        std::env::temp_dir().join(format!("sandrun-{case}-{}-{nonce}", std::process::id()))
    }

    #[test]
    fn resolves_absolute_symlinks_inside_rootfs() {
        let root = temporary_root("absolute-link");
        fs::create_dir_all(root.join("usr")).unwrap();
        fs::create_dir_all(root.join("opt/bin")).unwrap();
        fs::write(root.join("opt/bin/tool"), b"fixture").unwrap();
        symlink("/opt/bin", root.join("usr/bin")).unwrap();

        assert_eq!(
            resolve_target(&root, Path::new("/usr/bin/tool")).unwrap(),
            root.join("opt/bin/tool")
        );
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn rejects_symlink_escape_and_loop() {
        let root = temporary_root("invalid-link");
        fs::create_dir_all(&root).unwrap();
        symlink("../../outside", root.join("escape")).unwrap();
        symlink("/loop-b", root.join("loop-a")).unwrap();
        symlink("/loop-a", root.join("loop-b")).unwrap();

        assert!(matches!(
            resolve_target(&root, Path::new("/escape")),
            Err(RuntimeError::InvalidMountTarget { .. })
        ));
        assert!(matches!(
            resolve_target(&root, Path::new("/loop-a")),
            Err(RuntimeError::InvalidMountTarget { .. })
        ));
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn rejects_resolved_root_and_standard_mount_aliases() {
        let root = temporary_root("mount-alias");
        for directory in ["proc", "dev", "sys", "tmp", "run", "etc"] {
            fs::create_dir_all(root.join(directory)).unwrap();
        }
        symlink("/", root.join("root-alias")).unwrap();
        symlink("/etc", root.join("etc-alias")).unwrap();

        let mut config = Config {
            rootfs: root.clone(),
            overlay_device: None,
            overlay_id: None,
            workdir: PathBuf::from("/"),
            user: None,
            standard_mounts: false,
            binds: Vec::new(),
            tmpfs: vec![PathBuf::from("/root-alias")],
            command: vec![OsString::from("/bin/app")],
        };
        assert!(matches!(
            validate_resolved_mount_targets(&root, &config),
            Err(RuntimeError::InvalidMountTarget { .. })
        ));

        config.standard_mounts = true;
        config.tmpfs = vec![PathBuf::from("/etc-alias")];
        assert!(matches!(
            validate_resolved_mount_targets(&root, &config),
            Err(RuntimeError::Operation {
                operation: "resolved_mount_targets_invalid",
                ..
            })
        ));

        fs::remove_dir(root.join("etc")).unwrap();
        symlink("/", root.join("etc")).unwrap();
        config.tmpfs.clear();
        assert!(matches!(
            validate_resolved_mount_targets(&root, &config),
            Err(RuntimeError::InvalidMountTarget { .. })
        ));
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn escapes_overlay_option_delimiters() {
        let options = OsStringBuffer::overlay(
            Path::new("/lower:one"),
            Path::new("/upper,two"),
            Path::new("/work\\three"),
        );
        assert_eq!(
            options.as_os_str().as_bytes(),
            b"lowerdir=/lower\\:one,upperdir=/upper\\,two,workdir=/work\\\\three"
        );
    }

    #[test]
    fn overlay_identity_is_stable_and_unambiguous() {
        let first = overlay_identity(OsStr::new("fastapi"), Path::new("/mnt/fastapi"));
        let repeated = overlay_identity(OsStr::new("fastapi"), Path::new("/mnt/fastapi"));
        let different_id = overlay_identity(OsStr::new("egress"), Path::new("/mnt/fastapi"));
        let different_root = overlay_identity(OsStr::new("fastapi"), Path::new("/mnt/egress"));

        assert_eq!(first, repeated);
        assert_ne!(first, different_id);
        assert_ne!(first, different_root);
        assert_eq!(hex_lower(&Sha256::digest(first)).len(), 64);
    }

    #[test]
    fn resolves_named_user_from_passwd() {
        let passwd = "\
root:x:0:0:root:/root:/bin/sh
app:x:65532:65530:Application:/home/app:/bin/sh
";
        assert_eq!(
            parse_passwd(passwd, "app").unwrap(),
            UserIdentity {
                uid: 65532,
                gid: 65530,
            }
        );
        assert!(matches!(
            parse_passwd(passwd, "missing"),
            Err(RuntimeError::UserNotFound(user)) if user == "missing"
        ));
    }

    #[test]
    fn rejects_ambiguous_or_invalid_matching_passwd_entries() {
        assert!(matches!(
            parse_passwd("app:x:1:1::/:/bin/sh\napp:x:2:2::/:/bin/sh\n", "app"),
            Err(RuntimeError::InvalidPasswdEntry {
                reason: "user name is duplicated",
                ..
            })
        ));
        assert!(matches!(
            parse_passwd("app:x:not-a-uid:1::/:/bin/sh\n", "app"),
            Err(RuntimeError::InvalidPasswdEntry {
                reason: "UID is invalid",
                ..
            })
        ));
        assert!(matches!(
            parse_passwd("app:x:4294967295:1::/:/bin/sh\n", "app"),
            Err(RuntimeError::InvalidPasswdEntry {
                reason: "UID is invalid",
                ..
            })
        ));
    }
}
