# Sandcamp 技术方案

## 1. 项目定位

Sandcamp 只解决一个问题：让用户主进程和少量 Image Volume Sidecar 在同一个
AGS Linux 沙箱中正确启动。

Go 包只负责校验启动声明和生成请求配置，不替代官方 AGS SDK，也不包含
控制面客户端。AGS API 版本、重试、鉴权、实例生命周期和连接语义都继续由官方
客户端负责。

第一阶段以两类 Sidecar 为参考场景：

- 提供 HTTP/WebSocket 代理的 Python FastAPI 完整根文件系统；
- 按需修改沙箱共享网络策略的 OpenSandbox Egress。

## 2. 能力边界

Sandcamp 负责：

- 按确定顺序启动进程；
- 显式声明可执行文件、参数、环境变量和外层工作目录；
- 执行有明确时间上限的 HTTP 启动探测；
- 生成一份 AGS `CustomConfiguration`；
- 转发信号并管理直接子进程；
- 将 Sidecar 切换到自己的只读 OCI 根文件系统；
- 在该根文件系统中配置显式的读写、只读和 tmpfs 挂载。

Sandcamp 不负责：

- AGS Tool 或 Instance 的增删改查；
- Pause、Resume、Timeout、Connect、List 或 Token API；
- 查询镜像 Registry 或 Hub 元数据；
- 自动解析镜像的 `ENTRYPOINT`、`CMD`、`Env` 或 `User`；
- 分配 cgroup 或限制单进程 CPU/内存；
- PID、Network、User 或 IPC Namespace 隔离；
- 重启策略、日志查询 API 或第二套健康控制面；
- Docker-in-Docker。

## 3. 核心组件

### Go SDK 渲染逻辑

Go SDK 提供两个纯函数，整个过程不产生网络请求：

- `RenderMounts(images)` 生成只读 Runtime/Sidecar Image Volume 挂载；
- `RenderStart(images, spec)` 校验进程声明并生成官方 AGS
  `CustomConfiguration`。

`RenderStart` 的结果仅包含：

- `Command`：Image Volume 中 `campd` 的绝对路径；
- `Args`：一个无副作用的 `--` 标记，确保 AGS 替换主镜像继承的 `CMD`，
  而不是将其追加到 campd 参数；
- `Env`：一个 Base64 编码、带版本号的 `SANDCAMP_SPEC`；
- `Ports`：声明中唯一、需要对外暴露的 TCP 端口；
- `Probe`：符合 AGS 限制的 `HTTP GET /ready`，端口为 `49982`。

Go SDK 不生成 AGS 凭证、Tool ID、Instance 参数、Metadata 或生命周期调用。

AGS 会将实例配置合并到 Tool 默认配置中，因此必须使用非空的 `Args` 标记覆盖
镜像原有的 `CMD`。当实例端口列表为空时，AGS 不会清空 Tool 端口；如果声明没有
暴露端口，调用方必须确保所选 Tool 的端口配置本身可接受。

这里的顶层 `Args=["--"]` 与 sandrun 命令中的 `--` 不是同一层参数：前者只用于
AGS 配置覆盖，后者用于分隔 sandrun 选项和 Sidecar 命令。两者均由
`RenderStart` 生成，调用方只声明 `Process.Command`。

### campd

`campd` 是一个静态 Linux 二进制，按以下流程运行：

1. 校验并解码 `SANDCAMP_SPEC`；
2. 在 `0.0.0.0:49982` 启动初始状态为未就绪的 HTTP 服务；
3. 依次启动 Sidecar；
4. 等待每个可选的 Loopback HTTP 启动探测；
5. 最后启动主进程，并等待其可选启动探测；
6. 将整体状态锁定为就绪；
7. 关闭时向每个进程组转发信号；
8. 任一声明进程退出后结束运行。

Campd 不重启进程，也不会在启动完成后持续探测各进程。启动完成后，AGS 只观察
campd 的 `/ready`。这样可以保持与 AGS 的 HTTP 探活模型一致，不额外引入 TCP、
Exec、Threshold 或观察面 API。

所有启动探测按顺序执行。`RenderStart` 将探测总预算限制为 25 秒，为 AGS 的 30 秒
就绪期限预留 5 秒，用于进程创建和调度。

环境变量按进程类型处理：

- 主进程继承 campd 的环境，因此保留主镜像 OCI `ENV`、Tool Env 和 Instance
  `CustomConfiguration.Env`，再由主进程自己的 `Process.Env` 覆盖同名变量；
- Sidecar 启动前清空 campd 的继承环境，只注入自己的 `Process.Env`；
- Image Volume 不会应用 Sidecar 镜像 OCI Config 中的 `ENV`，调用方必须先解析
  并将必要变量（包括 `PATH`）显式写入 `Process.Env`；
- `SANDCAMP_SPEC` 不会传入任何子进程。

### sandrun

`sandrun` 是独立的可执行文件和命令行协议。`campd` 只把它视为普通命令，
不会解析根文件系统或挂载参数。

每次调用时，sandrun 会：

1. 在切换 Namespace 前校验全部源路径和目标路径；
2. 创建独立的 Mount Namespace，并将挂载传播设为 Private；
3. 按需将 AGS 本地 ext4 系统盘 `/dev/vda` 挂载到不会遮住 Image Volume 的
   sandrun 内部路径；
4. 使用稳定 Sidecar Identity 的 SHA-256 选择独立 `upperdir`/`workdir`，
   以只读 Image Volume 为 `lowerdir` 挂载 OverlayFS；
5. 应用声明中的 Bind 和 tmpfs 挂载；
6. 按需挂载 `/proc`、沙箱 `/dev`、只读 `/sys`、可写 `/tmp` 和 `/run`，
   并只读注入运行时生成的 DNS 和 Hosts 文件；
7. 使用 `pivot_root` 切换根文件系统；
8. 切换到 Sidecar 工作目录并 `exec` 目标命令。

这里采用 `pivot_root` 完成类似 chroot 的根目录切换，是因为它能在挂载准备完成后
断开旧根目录。该机制只保证依赖兼容，不构成安全隔离边界。

## 4. 启动链路

调用方先在 Sandcamp 外部解析全部镜像元数据，再发起普通的 AGS 启动请求，请求中
包含：

- 由调用方管理的 Tool 和 Instance 字段；
- Image Volume 挂载，例如：
  - `/mnt/sandcamp`：静态 Runtime 二进制；
  - `/mnt/fastapi`：FastAPI OCI 根文件系统；
  - `/mnt/egress`：Egress OCI 根文件系统；
- `sandcamp.RenderStart` 返回的 `CustomConfiguration`。

AGS 从 Runtime Image Volume 中启动 `campd`。一个典型的 Sidecar 命令如下：

```text
/mnt/sandcamp/bin/sandrun
  --rootfs /mnt/fastapi
  --overlay-device /dev/vda
  --overlay-id fastapi
  --workdir /opt/fastapi-proxy
  --standard-mounts
  --
  /usr/local/bin/python /opt/fastapi-proxy/app.py
```

`--` 后面的命令在 FastAPI 根文件系统中解析。因此 Python、Dynamic Loader、
动态库和 Python 包都来自同一个 OCI 镜像，而不是主镜像。这是 `RenderStart` 生成的
内部命令行；调用方只需填写 Sidecar 的 `Process.Command` 和 `Process.WorkDir`。

## 5. 只读 Image Volume

OCI Image Volume 包含镜像层文件，但通常不包含容器运行时生成的文件，尤其是
`/etc/hosts` 和 `/etc/resolv.conf`；同时整个 Volume 为只读。

`sandrun` 将整个 Sidecar 根目录挂为 OverlayFS，使 `/var`、`$HOME`、`/etc`
及其他普通镜像路径具有 Copy-on-Write 语义。当前 AGS 集成使用 `/dev/vda` 作为
ext4 写层介质；Sandcamp 在自己的 Mount Namespace 内挂载该设备，不在 `/mnt`
上挂载，因此不会遮住 Image Volume。

写层目录采用
`/sandcamp/overlay/v1/<sha256(overlay-id + canonical-rootfs)>` 命名，并保存完整
Identity 做碰撞校验。同一 Sidecar 重启复用写层；文件锁禁止并发使用同一个
`upperdir`/`workdir`；每次启动使用独立 `merged` 挂载点。当前生命周期策略不会
并发重启相同 Identity；锁 FD 和历史空 `merged` 目录均随业务进程或 Instance
回收，不作为跨 Instance 的持久资源。

`sandrun --standard-mounts` 处理常见运行目录：

- `/tmp`：模式为 `1777` 的可写 tmpfs；
- `/run`：模式为 `0755` 的可写 tmpfs；
- `/etc`：在 OverlayFS upper 中创建缺失目标，再只读挂载沙箱的 `hosts` 和
  `resolv.conf`，不再复制整棵目录；
- `/proc`、`/dev`、`/sys`：挂载或复用沙箱的运行时视图。

应用专用的可写路径必须显式声明：

```text
--bind /mnt/share /mnt/share
--ro-bind /mnt/config/proxy.json /etc/proxy.json
--tmpfs /var/cache/proxy
```

源路径位于主沙箱文件系统中，目标路径在 Sidecar 根文件系统内解析，并且必须提前
存在，文件或目录类型也必须一致。

## 6. 网络行为

Sandrun 不创建 Network Namespace。主进程、FastAPI 和 Egress 共享 Loopback、
网卡、路由和 Netfilter 状态。

因此：

- FastAPI 可以代理到 `127.0.0.1:<main-port>`；
- campd 可以通过 Loopback 检查所有 Sidecar；
- 启用 Egress 时，其 `iptables` 或 `nftables` 规则对整个沙箱生效；
- 只有启用 Egress 的沙箱才需要保留相应网络 Capability。

## 7. 权限与用户

Runtime Image Volume 中的受限 setuid launcher 只允许 PID 1 调用固定
`campd.real`，使 OCI `USER` 为非 root 的主镜像仍能以 root 完成 Bootstrap。
`sandrun` 挂载 `/dev/vda` 和 OverlayFS 需要 root 与 `CAP_SYS_ADMIN`；启用
Egress 时还需要 `CAP_NET_ADMIN`。

Main 的 `Process.User` 使用数字 UID/GID，由 campd 在 `exec` 前降权。Sidecar
可使用 `&ProcessUser{Name: "app"}`；sandrun 从镜像只读 lower 的
`/etc/passwd` 解析 UID 和主 GID，完成 OverlayFS、挂载与 `pivot_root` 后再
清空 Supplementary Groups 及 Capabilities、切换身份并启用
`PR_SET_NO_NEW_PRIVS`。用户不存在或 passwd 条目非法时不会回退 root。

root 与 `65532:65532` Main、Sidecar 命名用户均包含在集成测试范围内。共享 Bind
保留真实 UID/GID 和文件模式，不进行隐式身份映射。

## 8. 失败语义

- 非法声明会在发送 AGS 请求前失败。
- 非法运行时 Payload 会使 campd 在就绪前退出。
- 任一进程在启动阶段退出，整体启动失败。
- 启动探测超时会终止全部进程，并保持 `/ready` 为未就绪。
- 就绪后任一声明进程退出，campd 会先将 `/ready` 置为未就绪，再终止其他进程
  并退出。
- `SIGTERM`、`SIGINT`、`SIGHUP` 和 `SIGQUIT` 会转发到各进程组；超过固定宽限期
  后，campd 会升级为 `SIGKILL`。

AGS 始终是外层生命周期的最终管理者。

## 9. 兼容性说明

主镜像和 Sidecar 可以使用不同的 Linux 发行版和用户态依赖版本，但仍必须：

- 使用沙箱支持的 CPU 架构；
- 兼容宿主 Linux Kernel ABI；
- 包含声明的可执行文件和挂载目标路径；
- 兼容文档约定的只读 lowerdir 和 OverlayFS Copy-on-Write 行为；
- 能在 AGS 授予的 Capability 下运行。

这些是 OCI/Linux 兼容要求，不依赖主镜像使用的发行版或 libc。
