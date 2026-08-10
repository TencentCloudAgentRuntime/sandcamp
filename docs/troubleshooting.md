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
- UID/GID 是否为数字；
- Probe Path、端口和时间是否有效。

### `encoded startup declaration is too large`

编码后的 `SANDCAMP_SPEC` 超过 Sandcamp 的 120KiB 产品上限。

处理方式：

- 删除重复环境变量；
- 将证书、规则或大 JSON 改为只读 Bind 文件；
- 每个 Sidecar 只保留自己需要的 Env；
- 调整上限前先在目标运行环境中执行边界验证。

不要把上限直接设为 128KiB。Linux 对单个 `argv`/`envp` 字符串存在约 128KiB
边界，编码和平台启动字段还需要额外余量。

### `startup probes exceed the AGS readiness budget`

所有进程的 ReadyTimeout 串行相加超过 25 秒。减少探测数量或缩短超时，避免超过
AGS 30 秒 `/ready` 门禁。

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

`--overlay-device` 路径不存在。AGS 当前使用 `/dev/vda`。

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
Nested Overlay。不要使用主容器普通目录作为 upper；应使用 `/dev/vda` 的 ext4。

### `overlay_workspace_busy`

同一个 `overlay-id + rootfs` 已被另一个进程使用。每个并发 Sidecar 必须使用不同
`overlay-id`。进程重启可以复用原 ID。

## 5. RootFS 或命令失败

### `executable ... does not exist`

对于 Sidecar，`Process.Command[0]` 必须存在于该 Sidecar Image Volume 的根文件
系统中，而不是主镜像中。调用方只填写完整的 `Process.Command`；SDK 会在内部把
它放到 sandrun 的 `--` 分隔符之后，不应把 `--` 或 sandrun 参数写入
`Process.Command`。

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
由 campd 处理；SDK 会把 Sidecar 的 WorkDir 转换为内部 sandrun `--workdir`
参数，使其在 `pivot_root` 后解析。调用方不应自行拼接 `--workdir`。

## 6. Bind 失败

### Source/Target 类型不匹配

目录只能 Bind 到目录，文件只能 Bind 到文件。Target 必须提前存在于 Sidecar
镜像中。

### 只读 Bind 仍可写

确认使用 `--ro-bind` 而不是 `--bind`，并检查目标是否被后续挂载覆盖。

### 非 root Main 无法写共享文件

Bind 不执行 UID/GID 映射。检查：

```bash
stat -c '%u:%g %a %n' /shared /shared/file
```

通过目录属主、Group 或 Mode 显式允许跨用户访问。

## 7. 环境变量不符合预期

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

## 8. Probe 与端口

### Sidecar 已监听但 Instance 未 RUNNING

检查 StartupProbe 的 Path、Port 和响应时间。探测从共享 Network Namespace 的
Loopback 发起，只接受 HTTP 成功响应。

### Tool 默认端口未被清空

Spec 没有任何 `Expose` 时，Sandcamp 故意不发送 Ports 覆盖，AGS 会保留 Tool
默认端口。需要空端口 Tool 时，应在 Tool 层正确配置。

### Egress 不应暴露端口

Egress `24774` 仅用于 Loopback 健康检查时，不需要加入 `Expose`。

## 9. 退出和信号

- campd 将 `SIGTERM`、`SIGINT`、`SIGHUP`、`SIGQUIT` 转发到所有进程组。
- 超过宽限期后升级为 `SIGKILL`。
- sandrun 最终直接 `execve` Sidecar，不增加中间转发进程。
- 任一声明进程退出会导致整个 Sandcamp 运行结束，这不是自动重启策略。

如应用使用 Shell Wrapper，应在脚本末尾使用 `exec`，避免信号停留在 Shell：

```sh
exec /app/server "$@"
```

## 10. 收集问题信息

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
