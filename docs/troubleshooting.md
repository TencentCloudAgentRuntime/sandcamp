# Sandcamp 排障手册

## 1. 排障顺序

1. 先在调用方检查 `sandcamp.RenderMounts` 或 `sandcamp.RenderStart` 错误。
2. 确认 Tool 和 Instance 回读中的 StorageMount/MountOption。
3. 查看 campd 标准错误输出。
4. 根据错误前缀判断是声明、权限、挂载、根文件系统还是启动探测。
5. 使用同一组镜像执行本地 Linux 集成测试和目标 AGS 环境对照验证。

## 2. `RenderMounts` 或 `RenderStart` 在本地失败

### `invalid startup declaration`

检查：

- Process Name 是否重复或包含非法字符；
- Command 第一个元素是否为规范化绝对路径；
- Env Key 是否合法，Value 是否包含 NUL；
- WorkDir 是否为规范化绝对路径；
- User 是否只选择了一种模式：有效 Name，或非保留的数字 UID/GID；
- Kind、Probe 与 Timeout 的组合是否匹配；
- 声明中是否至少有一个 Service。

### `encoded startup declaration is too large`

编码后的 `SANDCAMP_SPEC` 超过 Sandcamp 的 120KiB 产品上限。

处理方式：

- 删除重复环境变量；
- 将证书、规则或大 JSON 改为只读 Bind 文件；
- 每个 Sidecar 只保留自己需要的 Env；
- 调整上限前先在目标运行环境中执行边界验证。

不要把上限直接设为 128KiB。Linux 对单个 `argv`/`envp` 字符串存在约 128KiB
边界，编码和平台启动字段还需要额外余量。

### `process startup exceeds the AGS readiness budget`

所有 Probe `StartupTimeout` 与 RunToCompletion `Timeout` 的声明预算之和超过
25 秒。减少需要串行等待的 Gate 或缩短上限，避免超过 AGS 30 秒 `/ready` 门禁。

## 3. Instance 无法启动

### `FailedOperation.ContainerStart`

这是 AGS 外层通用错误，不足以定位原因。重点检查 campd 标准错误：

- Runtime 是否挂载到 `/mnt/sandcamp`；
- `/mnt/sandcamp/bin/campd` 是否为 setuid launcher；
- `/mnt/sandcamp/bin/campd.real` 和 `sandrun` 是否存在并可执行；
- Image Volume 是否 `ReadOnly=true`；
- `SANDCAMP_SPEC` 是否存在；
- 主镜像是否允许 setuid Bootstrap 获得 EUID 0；
- PID 1 是否确实为 launcher。

### `campd-launcher: refusing non-PID-1 invocation`

launcher 只能作为容器 PID 1 使用。不要从 Shell 或其他辅助进程直接调用它。

### `setuid bootstrap did not obtain effective UID 0`

检查：

- Runtime Mount 是否带 `nosuid`；
- 容器是否启用 `no_new_privs`；
- 容器运行时是否保留 setuid 文件模式与 root 属主；
- 启动命令是否指向 launcher 而不是 `campd.real`。

## 4. sandrun 挂载失败

### `overlay_device_resolve_failed`

`--overlay-device` 路径不存在。SDK 默认使用 `/dev/vdb`；如果业务 sandbox 系统盘
位于其他设备，应通过 `Process.OverlayDevice` 显式覆盖。

### `overlay device is not a block device`

传入了普通文件或目录。该参数必须指向 ext4 Block Device。

### `overlay_device_mount_failed`

常见原因：

- campd/sandrun 不是 root；
- 缺少 `CAP_SYS_ADMIN`；
- 设备不是 ext4；
- 设备不可见或被平台限制。

### `overlay_mount_failed: Invalid argument`

通常表示 `upperdir`/`workdir` 位于另一个 OverlayFS 的 merged 层，形成不支持的
Nested Overlay。不要使用主容器普通目录作为 upper；应使用业务 sandbox 系统盘的
ext4/XFS 文件系统。

### `overlay_workspace_busy`

同一个 `overlay-id + rootfs` 已被另一个进程使用。每个并发 Sidecar 必须使用不同
`overlay-id`。进程重启可以复用原 ID。

### `Devices cgroup isn't mounted`

嵌套 dockerd 看不到 cgroup v1 controller 子挂载或 cgroup v2 unified mount。新版
sandrun 的标准挂载会自动递归映射 cgroup；分别从 Main 和 Sidecar 检查：

```bash
findmnt -R /sys/fs/cgroup
```

标准挂载只映射调用方已有的 cgroup 视图。如果 Source 本身只读、平台没有委托可写
子树，或缺少所需 Capability，dockerd 仍可能启动失败或无法创建容器。

Docker 使用 `overlay2` 时应配置 `DiskMounts: []string{"/var/lib/docker"}`，或将该
路径 Bind 到真实 XFS/ext4 StorageMount。若 `stat -f -c '%T' /var/lib/docker` 返回
`overlayfs`，仍存在 Nested Overlay。

## 5. RootFS 或命令失败

### `executable ... does not exist`

对于 Sidecar，`Process.Command[0]` 必须存在于该 Sidecar Image Volume 的根文件
系统中，而不是主镜像中。调用方只填写完整的 `Process.Command`。campd 会构造
sandrun argv，并在内部把业务命令放到 `--` 分隔符之后；公开 SDK 中没有需要调用方
填写的 `--`，也不应把 sandrun 参数写入 `Process.Command`。

检查：

- OCI Entrypoint/Cmd 是否完整转换；
- RootFS MountPath 是否正确；
- 绝对符号链接的目标是否存在于 Sidecar RootFS；
- 镜像 CPU 架构是否与沙箱一致。

### `No such file or directory`，但文件存在

通常是 Dynamic Loader 缺失，例如：

- glibc：`/lib64/ld-linux-x86-64.so.2`；
- musl：`/lib/ld-musl-x86_64.so.1`。

应挂载完整 OCI RootFS，不要只复制应用二进制。

### WorkDir 失败

Main 和 Sidecar 都通过 `Process.WorkDir` 声明镜像内工作目录。Main 的 WorkDir
由 campd 直接处理；Sidecar 的 WorkDir 由 campd 转换为 sandrun `--workdir`，在
`pivot_root` 后解析。调用方不应自行拼接 `--workdir`。

### `user_not_found` 或 `invalid_passwd_entry`

命名 Main 用户从主镜像 `/etc/passwd` 解析；命名 Sidecar 用户从对应 Sidecar
immutable lower RootFS 的 `/etc/passwd` 解析。确认名称位于正确镜像中，匹配条目
恰好有七个字段，UID/GID 是有效 `uint32` 且不是 `4294967295`。Sandcamp 不查询
NSS/LDAP，也不会回退 root。

如果应用只需要固定的文件权限身份，可以改用成对的数字 UID/GID；数字模式不查询
passwd，也不会创建 Home 目录或设置用户环境变量。

## 6. Init Job 失败

### `run-to-completion process ... failed`

Job 必须在 `Process.Timeout` 内以 0 退出。非零退出、被信号终止、超时，或退出后在
原进程组留下后代，都会中止后续声明。已经启动的 Service 不会被清理，但 campd
`/ready` 会保持 503。

Job 超时时 campd 先向该 Job 进程组发送 SIGTERM，5 秒后仍存在则发送 SIGKILL。
检查 Job 是否使用 `exec`、是否错误地启动后台进程，以及 Timeout 是否覆盖真实耗时。

## 7. Bind 失败

`Process.Mounts[].Source` 在主 Mount Namespace 中解析，`Target` 在对应 Sidecar RootFS
中解析。Main 不能配置 `Mounts`。

### Source 自动创建失败

Source 不存在时会连同缺失父目录自动创建为 `0777` 目录。若其父路径不可写、位于只读
挂载，或路径中已有普通文件，会创建失败。缺失 Source 不会自动创建为普通文件；文件
Source 必须预先提供。

### Source/Target 类型不匹配

目录只能 Bind 到目录，文件只能 Bind 到文件。Target 不存在时会根据 Source 类型在
Sidecar OverlayFS 中自动创建；Target 已存在时必须与 Source 类型一致且不会被修改。

### Target 与标准挂载重叠

不能把 Target 放在 `/tmp/cache`、`/run/app` 等标准挂载内部，也不能覆盖
`/etc/hosts` 或 `/etc/resolv.conf`。选择独立路径，例如 `/mnt/share` 或 `/data`。

### `mount` 把 Bind 显示为 `overlay` 或 `overlay2`

这是正常现象。Bind Mount 不创建新文件系统，只为 Source 对应的 VFS 子树增加一个挂载
引用；`mount` 通常显示 Source 所属的底层文件系统，而不是原始 Source 路径。例如 Main
RootFS 是 OverlayFS 时，Sidecar 内可能显示 `overlay2 on /var/tmp type overlay`。

可以读取目标进程的 `mountinfo`。Bind 记录的 root 字段会是 Source 在底层文件系统中的
路径，mount point 字段则是 Sidecar Target：

```bash
grep ' /var/tmp ' /proc/$sidecar_pid/mountinfo
```

再分别从 Main 和 Sidecar 对同一文件执行 `stat -c '%d:%i'`；device 与 inode 相同即可
确认双方看到的是同一个 Bind 文件，而不是两份复制数据。

### 只读 Bind 仍可写

确认使用 `--ro-bind` 而不是 `--bind`，并检查目标是否被后续挂载覆盖。

### 非 root Main 无法写共享文件

Bind 不执行 UID/GID 映射。检查：

```bash
stat -c '%u:%g %a %n' /shared /shared/file
```

通过目录属主、Group 或 Mode 显式允许跨用户访问。

## 8. 环境变量不符合预期

### Sidecar 缺少主镜像 Env

这是预期行为。Sidecar 会清空继承环境，只接收 `Process.Env`。必须显式提供：

- `PATH`；
- `HOME`；
- Locale；
- Proxy/CA；
- 应用配置。

### Main Env

Main 按以下优先级合并：

1. 主镜像 OCI Env；
2. Tool/Instance Env；
3. `Process.Env` 覆盖同名变量。

`SANDCAMP_SPEC` 会在启动子进程前移除。

## 9. Probe 与端口

### Sidecar 已监听但 Instance 未 RUNNING

检查 `Process.Probe` 的 Path、Port、StartupTimeout 与阈值。探测从共享 Network
Namespace 的 Loopback 发起，HTTP 200–399 视为成功。首次达到成功阈值前，后续声明
不会启动。

### Instance 已 RUNNING，之后 `/ready` 变为 503

Readiness 会持续执行，不只负责启动。以下任一情况都会使聚合状态变为 503：

- Service 进程退出；
- Service Probe 连续失败达到 FailureThreshold；
- 初始化 Job 或后续启动 Gate 失败。

Probe 恢复并达到 SuccessThreshold 后可以重新变为 200；退出的 Service 不会被
campd 自动重启。

### Probe 通过，但目标进程实际未 Ready

Probe 的公开语义是共享 Loopback 上的 HTTP 端点状态，不是进程身份。检查 Probe
端口和路径是否指向预期端点，以及是否有较早启动的进程监听或代理了该地址。若业务
要求 PID 级身份确认，需要使用业务自己的协议；Sandcamp Probe 不提供该保证。

### Tool 默认端口未被清空

Spec 没有任何 `Expose` 时，Sandcamp 故意不发送 Ports 覆盖，AGS 会保留 Tool
默认端口。需要空端口 Tool 时，应在 Tool 层正确配置。

### Egress 不应暴露端口

Egress `24774` 仅用于 Loopback 健康检查时，不需要加入 `Expose`。

## 10. 退出和信号

- campd 将 `SIGTERM`、`SIGINT`、`SIGHUP`、`SIGQUIT` 转发到所有进程组。
- 超过宽限期后升级为 `SIGKILL`。
- sandrun 最终直接 `execve` Sidecar，不增加中间转发进程。
- 单个 Service 退出只会令 `/ready` 变为 503，并清理该进程组的残留后代；其他
  Service 保持运行。
- campd 不自动重启退出进程。
- campd 只保证清理原进程组；主动 `setsid` 的后代不在该保证内。沙箱整体回收由
  外层运行时负责。

如应用使用 Shell Wrapper，应在脚本末尾使用 `exec`，避免信号停留在 Shell：

```sh
exec /app/server "$@"
```

## 11. 收集问题信息

提交问题时至少提供：

- Sandcamp Commit/Release Tag；
- Runtime、Sidecar、主镜像 Digest；
- AGS 状态和错误码；
- 脱敏后的 `Spec` 结构和编码长度；
- Tool/Instance Mount 回读；
- campd/sandrun 完整错误前缀；
- 可以复现问题的最小化命令。

不要提交 SecretId、SecretKey、API Key、AccessToken、TrafficToken、私钥或完整
`SANDCAMP_SPEC`；Region、Tool ID、Instance ID 和镜像仓库命名空间也应按需脱敏。
