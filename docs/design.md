# Sandcamp 技术方案

## 目标与边界

Sandcamp 解决一个具体问题：在同一个 AGS Linux 沙箱中，按确定顺序启动主镜像进程
和 Image Volume Sidecar，同时让 Sidecar 使用自己的完整 OCI RootFS。

Sandcamp 负责：

- 校验结构化的镜像与进程声明；
- 生成 Image `StorageMounts` 与 AGS `CustomConfiguration`；
- 编排 Service 和 RunToCompletion Job；
- 持续聚合 HTTP Readiness；
- 为 Sidecar 准备 Mount Namespace、OverlayFS 与 `pivot_root`；
- 为 Main 与 Sidecar 应用镜像命名身份或显式数字身份；
- 在退出时清理进程组并转发外部信号。

Sandcamp 不负责 Tool/Instance API、鉴权、Registry 元数据解析、单进程重启、cgroup
分配，或额外的 PID/Network/User/IPC 隔离。

## 组件

| 组件 | 职责 |
| --- | --- |
| Go SDK | 声明校验、Image Volume 挂载渲染、AGS 启动配置渲染 |
| `campd-launcher` | 仅允许 PID 1 执行固定 `campd.real` 的受限 setuid Bootstrap |
| `campd` | PID 1、进程编排、持续 Readiness、子进程回收与信号处理 |
| `sandrun` | 单个 Sidecar 的 Mount Namespace、OverlayFS、用户切换与 exec |

`campd` 与 `sandrun` 是分离的静态二进制，但由同一个 Runtime 镜像一起发布和验证。
campd 负责生命周期，sandrun 保持为可独立验证的 RootFS 执行器。

## 从公开声明到运行时协议

公开 API 按镜像来源分组：

```go
type Spec struct {
    Sidecars []Process // 使用同名 Image Volume
    Main     []Process // 使用 AGS 主镜像
}
```

Renderer 按以下顺序转换：

1. 校验 `ImageSet` 与 `Spec`；
2. 通过 `Process.Name` 找到同名 `SidecarImage`；
3. 为 Sidecar 注入 SDK 管理的 MountPath、Overlay 设备和标准挂载开关；
4. 把 `Expose` 汇总为 AGS Ports；
5. 把剩余进程声明序列化为 runtime declaration v3；
6. Base64 编码后写入 `SANDCAMP_SPEC`。

runtime declaration 是 Go SDK 与 campd 之间的 wire 协议，不是第二套公开 API。v3
为 Main 和 Sidecar 统一了两种 User 形状；新 campd 仍接受既有 v2 声明。其形状
概念上如下：

```json
{
  "version": 3,
  "sidecars": [{
    "name": "proxy",
    "kind": "service",
    "rootfs": "/mnt/sandcamp-sidecars/proxy",
    "overlay_device": "/dev/vda",
    "standard_mounts": true,
    "command": ["/opt/proxy/server"],
    "user": {"name": "app"}
  }],
  "main": [{
    "name": "api",
    "kind": "service",
    "command": ["/app/server"],
    "user": {"uid": 65532, "gid": 65532}
  }]
}
```

Go SDK 不再把 Sidecar 提前编译成一条 sandrun 命令。campd 根据结构化 Sidecar 字段
构造：

```text
/mnt/sandcamp/bin/sandrun
  --rootfs /mnt/sandcamp-sidecars/proxy
  --overlay-device /dev/vda
  --overlay-id proxy
  --standard-mounts
  --workdir /opt/proxy
  --user app
  --
  /opt/proxy/server
```

这里的内部 `--` 只分隔 sandrun 选项与 Sidecar argv。调用方的
`Process.Command` 从 `/opt/proxy/server` 开始，不包含 sandrun 或 `--`。

`RenderStart` 返回的 AGS 顶层 `Args=['--']` 是另一层配置标记，用于阻止主镜像原有
CMD 被追加到 campd；它也不是业务进程参数。

## campd 生命周期

campd 启动后：

1. 解码并再次校验 runtime declaration；
2. 在 `0.0.0.0:49982` 启动初始为 503 的 `/ready`；
3. 设置 Child Subreaper 和信号处理器；
4. 在任何声明启动前，从主镜像 `/etc/passwd` 解析并缓存全部 Main 命名身份；
5. 把 Sidecars 与 Main 合并为“Sidecars 在前、Main 在后”的有序列表；
6. 为每个声明创建独立进程组，并按 Kind 执行启动 Gate；
7. 所有声明处理成功后，将初始化状态标记为完成；
8. 持续回收子进程、执行 Readiness Probe，并更新聚合状态。

Main 直接在主 Mount Namespace 中执行，可访问主镜像 RootFS 和 Tool StorageMount。
Sidecar 通过与 campd 同目录的 sandrun 执行。
campd 不依赖主镜像的 PATH 查找 Runtime 二进制。

### Service

- 无 Probe：`spawn` 成功即允许处理下一项；PID 存活代表该 Service Ready；
- 有 Probe：首次达到 SuccessThreshold 后允许处理下一项；
- 首次 Ready 后仍持续探测，FailureThreshold 和 SuccessThreshold 控制状态转换；
- Service 退出后变为 Unready，其他 Service 不会被终止；
- campd 不重启退出的 Service。

### RunToCompletion

- 必须在 Timeout 内退出；
- 退出码 0 才允许处理下一项；
- 非零退出、信号退出、超时或在原进程组中遗留后代都会使初始化失败；
- 超时 Job 的进程组先收到 SIGTERM，5 秒后仍存在则收到 SIGKILL；
- Job 不参与初始化完成后的聚合 Readiness。

声明中必须至少有一个 Service。这样 `/ready=200` 始终对应至少一个仍存活的长期
进程，而不是一组已经全部完成的 Job。

### 初始化失败

以下情况会停止处理后续声明：

- Job 非零退出、超时或遗留同组后代；
- 有 Probe 的 Service 未在 StartupTimeout 内首次 Ready；
- 任一已启动 Service 在初始化完成前退出；
- 新进程无法创建。

已经启动的 Service 保持运行，campd 保持存活，`/ready` 保持 503。无效 wire
Payload 无法建立可信运行时状态，因此会在 campd 启动早期直接返回错误。

## Readiness 聚合

整体 `/ready` 为 200 必须同时满足：

```text
初始化完成
AND 没有初始化失败
AND 每个 Service 仍存活
AND 每个带 Probe 的 Service 当前为 Ready
```

因此运行期间可以出现 `200 → 503 → 200`，而无需杀进程或重启 campd。Probe 是状态
观测，不是进程恢复策略。AGS 如何使用这个 HTTP 结果属于外层平台配置，Sandcamp
自身不依赖外层重启行为。

Probe 从共享 Network Namespace 的 `127.0.0.1` 发起。它表达“配置的 HTTP 端点
可用”，不表达“某个 Linux PID 已就绪”。多个进程共享 Loopback，因此响应者身份
不属于 Probe 协议。

## Sidecar RootFS

Image Volume 提供只读 OCI 文件树，但不会自动应用镜像 Config，也通常不含运行时生成
的 `/etc/hosts` 与 `/etc/resolv.conf`。

每次 sandrun 调用：

1. 在切换 Namespace 前校验 rootfs、设备、Bind 与命令；
2. 创建独立 Mount Namespace，并把挂载传播设为 Private；
3. 将 `/dev/vda` 的 ext4 文件系统挂到不会遮住 Image Volume 的运行时路径；
4. 以 Sidecar Image Volume 为 lowerdir 创建 OverlayFS；
5. 应用显式 Bind/tmpfs 和标准运行时挂载；
6. 执行 `pivot_root`，断开旧 RootFS；
7. 应用 WorkDir 与可选的命名或数字身份；
8. 直接 `execve` Sidecar 命令。

Overlay Identity 使用 `overlay-id + canonical rootfs` 的 SHA-256，写层位于
`/sandcamp/overlay/v1/...`。新建 upper 根目录继承 lower `/` 的 UID、GID 和 Mode，
避免把合并 RootFS 意外变成 `0700`。已经存在的 upper 不覆盖元数据，以保留 COW
状态。文件锁禁止同一 Identity 并发挂载。

标准挂载包括 `/proc`、沙箱 `/dev`、只读 `/sys`、可写 tmpfs `/tmp` 与 `/run`，以及
只读注入的 hosts/resolv.conf。应用共享目录仍需显式 Bind。

## 环境与用户

Main：

- 继承主镜像和 AGS 启动环境；
- `Process.Env` 覆盖同名值；
- exec 前移除 `SANDCAMP_SPEC`；
- `User.Name` 从主镜像 `/etc/passwd` 解析，且在任何进程启动前缓存；
- 数字 UID/GID 直接应用，不查询 passwd。

Sidecar：

- 启动前清空继承环境，只注入 `Process.Env`；
- `User.Name` 从自己的 immutable lower `/etc/passwd` 解析；
- 数字 UID/GID 直接传给 sandrun，不查询 passwd；
- 不支持 NSS/LDAP、`user:group`、附加组解析或自动环境变量。

两类进程共享以下身份语义：

- `User=nil` 继承运行时身份，通常为 root；
- 显式身份会设置主 GID/UID 并清空附加组；
- 非 root 会进一步清空四组 Capability 并设置 `PR_SET_NO_NEW_PRIVS`；
- 显式 `0:0` 或名称 `root` 保留 root Capability，且不设置 `no_new_privs`；
- 名称缺失、重复或 passwd 条目非法时直接失败，不回退 root。

## Namespace 与安全边界

Sidecar 只有独立 Mount Namespace。Main 与 Sidecar 共享：

- PID Namespace 与进程可见性；
- Network Namespace、Loopback、路由和 Netfilter；
- IPC、cgroup 和沙箱资源上限。

共享网络使 FastAPI 能访问 Main 的 Loopback，也使 Egress 规则作用于整个沙箱。代价
是 Probe 响应者无法仅通过端口区分。Mount Namespace、OverlayFS 与 `pivot_root`
用于提供依赖兼容，不是安全隔离机制。

## 退出与信号

受管 Service 自身退出时：

- campd 标记该 Service 已退出并令 `/ready` 返回 503；
- 清理仍留在该进程组中的后代；
- 其他 Service 保持运行；
- campd 继续作为 PID 1 回收子进程。

当 campd 收到 `SIGTERM`、`SIGINT`、`SIGHUP` 或 `SIGQUIT` 时，它向所有仍存活的受管
进程组转发同一信号，等待 5 秒，再对残留进程组发送 SIGKILL，并以 `128+signal`
退出。当前不保证 Main 与 Sidecar 的信号先后顺序。

进程组不是完整的后代容器。campd 只保证清理受管进程原进程组中的后代；主动调用
`setsid` 创建新 Session 的后代不在该保证内。沙箱整体销毁仍由外层运行时负责。
