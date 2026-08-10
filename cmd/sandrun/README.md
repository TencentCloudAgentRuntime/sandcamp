# sandrun

`sandrun` 是一个独立的根文件系统启动器，用于从 Sandcamp Image Volume 启动进程。
它能让普通进程使用单独挂载的 OCI 根文件系统解析绝对路径和动态依赖。

Sandrun 不依赖 `campd` 的 Crate 或运行时，可以直接调用，也可以由任意进程管理器
启动。

Mount Namespace 和 `pivot_root` 用于保证依赖正确：可执行文件、Dynamic Loader、
动态库和绝对路径都必须来自指定的根文件系统。它们不是安全隔离边界。

`sandrun` 并不是 OCI Runtime：

- 只创建 Mount Namespace；
- 继承调用方的 Network Namespace、PID、进程组、cgroup、环境变量、文件描述符
  和 Capabilities；
- 不负责镜像拉取或解包、cgroup 管理、Seccomp 配置或 Daemon 生命周期；
- 可按镜像 `/etc/passwd` 中的命名用户，在挂载完成后清理 Capabilities 并切换
  UID/GID；
- 根文件系统准备完成后直接调用 `execve`，不会额外 Fork，因此上层管理器仍然
  管理同一个 PID 和 cgroup。

## 平台要求

静态 Linux/amd64 二进制要求 Linux Host 允许
`unshare(CLONE_NEWNS)`、Bind Mount、tmpfs、`pivot_root` 和 Lazy Unmount。
因此，启动器需要有效的 `CAP_SYS_ADMIN`；Egress 进程还需要在 `exec` 后继续保留
`CAP_NET_ADMIN`。

安全约束将 Image Volume 根文件系统、Bind Source 和完整启动声明视为可信控制面
输入，不允许沙箱内的不可信进程直接构造。若允许更广泛的自定义 Bind，必须增加
源路径 Allowlist，并改用 `openat2` 等基于文件描述符的目标解析，避免符号链接
替换造成 TOCTOU。

## 使用方式

下面是直接调用 sandrun 的底层命令行；使用 Go SDK 时只填写
`Process.Command`、`Process.WorkDir` 和 `Process.User`，`RenderStart` 会生成这些
参数以及 `--` 分隔符。

```sh
sandrun \
  --rootfs /mnt/image-volumes/fastapi \
  --overlay-device /dev/vda \
  --overlay-id fastapi \
  --workdir /opt/fastapi-proxy \
  --user app \
  --standard-mounts \
  --bind /var/lib/sandcamp/fastapi /var/lib/fastapi \
  -- \
  /usr/local/bin/python /opt/fastapi-proxy/app.py
```

当前 AGS 集成将 ext4 系统盘暴露为 `/dev/vda`。`--overlay-device`
在 sandrun 自己的 Mount Namespace 中挂载该设备，用它承载 OverlayFS 的
`upperdir` 和 `workdir`；不会把设备挂到 `/mnt`，因此不会遮住 Image Volume。

`--overlay-id` 是同一沙箱内稳定且唯一的 Sidecar 身份。sandrun 使用
`SHA-256(overlay-id + canonical rootfs)` 选择写层目录，并保存完整 Identity
用于碰撞校验。同一 Identity 的进程重启会复用写层；非阻塞文件锁会拒绝两个进程
同时挂载同一个写层；每次启动的 `merged` 目录仍保持唯一。锁 FD 会跨
`execve` 继承，业务若主动关闭未知 FD 会提前释放该辅助锁；当前 campd 不会在同一
沙箱内并发重启相同 Identity，因此它不是运行正确性的唯一保障。空的历史
`merged` 目录随 Instance 系统盘一起回收。

`--standard-mounts` 提供：

- 新的 `/proc`；
- 调用方的 `/dev`；
- 调用方 `/sys` 的只读视图；
- `/tmp` 和 `/run` 的 tmpfs；
- 在 OverlayFS 写层中创建缺失的 `/etc/resolv.conf` 和 `/etc/hosts` 目标，
  再只读挂载调用方生成的对应文件。

OCI 镜像层通常不包含 `resolv.conf` 和 `hosts`；这些文件由容器运行时动态生成。
OverlayFS 允许 sandrun 在不修改只读 Image Volume 的前提下创建目标，不再复制
整棵 `/etc`。

显式声明的 `--bind` 和 `--ro-bind` 均为非递归挂载。标准 `/dev` 挂载是递归的，
以便保留 `devpts` 等设备子挂载。

除自动生成的 DNS 和 Hosts 文件外，所有显式挂载目标都必须提前存在于根文件系统
中。普通镜像路径的写入由 OverlayFS 自动 Copy-on-Write；需要与主进程共享或独立
持久化的数据仍应使用显式 `--bind`。

挂载镜像只会提供文件，不会提供 OCI 运行配置。`Entrypoint`、`Cmd`、`Env`、
`User`、`WorkingDir`、`Healthcheck` 和所需 Capabilities 都必须显式转换为进程
声明和 `sandrun` 参数。

### 命名用户

`--user app` 只接受用户名，不接受 UID、组名或 `user:group`。sandrun 从只读
lower RootFS 的 `/etc/passwd` 解析 UID 和主 GID，避免持久 Overlay upper 修改
后影响下一次启动身份。

执行顺序为：

1. 以 root 创建 OverlayFS、标准挂载和显式 Bind；
2. `pivot_root` 并进入 WorkDir；
3. 清空 Supplementary Groups 和 Ambient Capabilities；
4. 切换真实/有效/保存 GID 和 UID；
5. 清空剩余 Effective/Permitted/Inheritable Capabilities，设置
   `no_new_privs` 并立即 `exec`。

用户名不存在、passwd 条目非法或重复时返回 125，绝不回退为 root。该参数不查询
NSS/LDAP，也不自动设置 `HOME`、`USER` 或 `LOGNAME`。省略 `--user` 时保持原有
root 行为。

## 与进程管理器集成

`sandrun` 是命令行边界，不是 `campd` 内部 API。如果由 campd 等外部管理器启动，
管理器不应设置自己的工作目录，而应通过 `sandrun --workdir` 传入 Sidecar 内部
目录；否则，管理器会在 sandrun 切换根文件系统前解析工作目录。Sandcamp Go SDK
通过 Sidecar 的 `Process.WorkDir` 自动完成这项转换。

HTTP 启动探测通过继承的 Network Namespace 工作。在 sandrun 外启动的命令无法
访问只存在于指定根文件系统中的可执行文件。

## 验证

在安装 Musl Target 和 Docker 的 Linux/amd64 Host 上执行：

```sh
CARGO_TARGET_DIR=target/sandrun cargo build \
  --manifest-path cmd/sandrun/Cargo.toml \
  --release \
  --target x86_64-unknown-linux-musl

SANDRUN_BIN=target/sandrun/x86_64-unknown-linux-musl/release/sandrun \
  bash ./scripts/test-sandrun-linux.sh
```

Linux 验证脚本会将每个测试镜像导出为独立根文件系统，移除 `/etc/hosts` 和
`/etc/resolv.conf`，以只读方式挂载根文件系统，再由特权测试进程通过 sandrun
启动。脚本验证：

- `/var`、`$HOME` 和 `/etc` 的通用 Copy-on-Write 不修改只读 lowerdir；
- 稳定 Identity 在进程重启后复用写层，并拒绝同一写层的并发挂载；
- 运行时 DNS 和 Hosts 文件能注入 OverlayFS 根目录；
- 显式可写 Bind Mount 在切换根目录后仍然有效；
- 命名用户在挂载完成后获得预期 UID/GID、空附加组、零 Capabilities 和
  `NoNewPrivs=1`；
- 缺失用户会以明确错误失败，不回退 root；
- FastAPI Runtime 和反向代理使用自己的根文件系统；
- OpenSandbox Egress 在继承的 Network Namespace 中安装 DNS 重定向规则。
