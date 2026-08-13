# Sandcamp Go SDK API 参考

## 安装

仓库为 Private 时，先配置 GitHub 读取凭证和私有模块前缀，再引用精确版本：

```bash
go env -w GOPRIVATE=github.com/TencentCloudAgentRuntime
go get github.com/TencentCloudAgentRuntime/sandcamp@v0.1.0-beta
```

```go
import "github.com/TencentCloudAgentRuntime/sandcamp"
```

Sandcamp 返回官方 AGS Go SDK `v20250920` 的 `StorageMount` 和
`CustomConfiguration` 类型，但不会读取凭证、访问 Registry、创建 Tool 或启动
Instance。

## 核心调用

```go
mounts, err := sandcamp.RenderMounts(images)
if err != nil {
    return err
}
configuration, err := sandcamp.RenderStart(images, spec)
if err != nil {
    return err
}

toolRequest.StorageMounts = append(toolRequest.StorageMounts, mounts...)

toolConfiguration := *configuration
toolConfiguration.Image = &mainImage
toolConfiguration.ImageRegistryType = common.StringPtr(string(registryType))
toolRequest.CustomConfiguration = &toolConfiguration

// startRequest.CustomConfiguration 与 startRequest.MountOptions 保持 nil，
// 分别继承 Tool.CustomConfiguration 和 Tool.StorageMounts。
```

这里的 Renderer 只是指 `RenderMounts` 和 `RenderStart` 两个“把声明转换成请求字段”
的函数，不是独立服务。两者都是确定性的纯函数。custom Tool 要求默认 `Command` 和
`Probe`，所以 Tool 与 Instance 都使用 `RenderStart` 输出；Tool 副本额外补充主镜像、
Resources 等 Tool 字段。Instance 继承 Tool 的 `StorageMounts`。

## 镜像声明

```go
type ImageRegistryType string

const (
    ImageRegistryPersonal   ImageRegistryType = "personal"
    ImageRegistryEnterprise ImageRegistryType = "enterprise"
)

type Image struct {
    Reference         string
    ImageRegistryType ImageRegistryType
}

type SidecarImage struct {
    Name              string
    Reference         string
    ImageRegistryType ImageRegistryType
}

type ImageSet struct {
    SandcampRuntime Image
    Sidecars        []SidecarImage
}
```

`ImageSet` 只描述 Image Volume；AGS 主镜像仍配置在 Tool 的
`CustomConfiguration.Image`。

- `ImageRegistryPersonal` 的 AGS 值是 `personal`，对应腾讯云 CCR 个人版；
- `ImageRegistryEnterprise` 的 AGS 值是 `enterprise`，对应腾讯云 TCR 企业版；
- Runtime 和 Sidecar 的 `Reference` 必须指向所选类型的腾讯云 Registry；
- GHCR 等通用 OCI Registry 不能直接作为 AGS Image Volume，使用前需要把镜像同步到
  调用方自己的 CCR 或 TCR；
- Sidecar `Name` 必须符合进程名称规则且不可重复；
- Sidecar `Name` 必须与对应的 `Process.Name` 一致；
- 当前一个 Sidecar Image 声明对应一个 Sidecar Process；
- Reference 不允许为空、首尾空白或 NUL。

### `RenderMounts`

```go
func RenderMounts(images ImageSet) ([]*ags.StorageMount, error)
```

返回只读 Image `StorageMounts`：

| 镜像 | Mount Name | MountPath |
| --- | --- | --- |
| Runtime | `sandcamp-runtime` | `/mnt/sandcamp` |
| Sidecar `NAME` | `sandcamp-sidecar-NAME` | `/mnt/sandcamp-sidecars/NAME` |

Mount Name 与路径属于 Sandcamp 运行时契约，调用方直接使用返回值。

## 进程声明

```go
type Spec struct {
    Sidecars []Process
    Main     []Process
}

type ProcessKind string

const (
    Service         ProcessKind = "service"
    RunToCompletion ProcessKind = "run-to-completion"
)

type Process struct {
    Name    string
    Kind    ProcessKind
    Command []string
    Env     map[string]string
    WorkDir string
    User    *ProcessUser
    Expose  []int

    Probe   *ReadinessProbe
    Timeout time.Duration
}
```

处理顺序固定为 `Sidecars` 的声明顺序，随后是 `Main` 的声明顺序。`Main` 可以为空或
包含多个进程；其中每个进程都直接使用同一 AGS 主镜像 RootFS。整个声明至少包含一个
Service，不能只有 RunToCompletion Job。

### `Name`

- Main 与 Sidecar 都必须显式填写；
- 长度 1–64；
- 首字符必须是 ASCII 字母或数字；
- 后续只能包含字母、数字、点、下划线和连字符；
- 同一个 `Spec` 内不可重复；
- Sidecar 必须存在同名 `SidecarImage`。

### `Kind` 与生命周期

`Kind` 为空时默认为 `Service`。

Service：

- 可以配置 `Probe`，不能配置 `Timeout`；
- 有 Probe 时，首次达到成功阈值后才处理下一项；
- 没有 Probe 时，进程成功创建即通过启动 Gate；
- 初始化完成后，PID 存活与持续 Probe 状态共同决定聚合 `/ready`；
- Service 退出会让 `/ready` 变为 503，但不会终止其他 Service；
- campd 不自动重启 Service。

RunToCompletion：

- 必须配置 `Timeout`；
- 不能配置 `Probe` 或 `Expose`；
- 在 Timeout 内以 0 退出才会处理下一项；
- 非零退出或超时会使初始化失败，后续声明不再启动；
- 已经启动的 Service 保持运行，聚合 `/ready` 保持 503。

### `Command`

- 完整 argv，不是 Shell 字符串；
- `Command[0]` 必须是该进程 RootFS 中的规范化绝对路径；
- 不执行 Shell 展开、变量替换或引号解析；
- Main 路径在主 Mount Namespace 中解析，可以来自主镜像或 Tool StorageMount；
- Sidecar 路径来自同名 Sidecar Image Volume；
- 调用方不拼接 `sandrun` 参数，也不在 `Command` 中加入 `--`。

需要 Shell 时显式声明：

```go
Command: []string{"/bin/sh", "-c", "exec /app/server --port 8080"}
```

### `Env`

- Key 必须符合 `[A-Za-z_][A-Za-z0-9_]*`；
- Key 与 Value 不允许包含 NUL；
- 不允许覆盖 `SANDCAMP_SPEC`；
- Sidecar 启动前清空继承环境，只接收自己的 `Process.Env`；
- Main 保留主镜像与 AGS 环境，再由 `Process.Env` 覆盖同名变量；
- `SANDCAMP_SPEC` 不会传给子进程；
- Image Volume 不应用 OCI `ENV`，Sidecar 通常需要显式提供 `PATH`、`HOME` 等。

全部声明编码到一个 `SANDCAMP_SPEC`，Base64 编码后上限为 120 KiB。证书、大型策略
或配置文件应通过挂载传递。

### `WorkDir`

- Main：主 Mount Namespace 中的绝对路径，可以位于主镜像或 Tool StorageMount；
- Sidecar：同名 Sidecar 镜像中的绝对路径；
- `/` 可作为 WorkDir；
- Sidecar WorkDir 在 `pivot_root` 后应用。

### `User`

```go
type ProcessUser struct {
    Name string
    UID  uint32
    GID  uint32
}
```

Main 和 Sidecar 都可以使用数字 UID/GID：

```go
User: &sandcamp.ProcessUser{UID: 65532, GID: 65532}
```

两者也都可以使用进程所属镜像内的用户名：

```go
User: &sandcamp.ProcessUser{Name: "app"}
```

- `Name` 与数字 UID/GID 是互斥的两种模式；
- Main 名称从主镜像 `/etc/passwd` 解析；Sidecar 名称从对应 immutable lower
  RootFS 的 `/etc/passwd` 解析；
- Main 命名身份在任何声明启动前解析并缓存，Init Job 改写 passwd 不会改变后续身份；
- 数字身份不要求 passwd 条目，也不会创建用户、组或 Home 目录；
- `User=nil` 时 Main 与 Sidecar 都继承运行时身份，通常为 root；
- Sandcamp 不自动恢复主镜像 OCI `USER`；
- 主 GID 取 passwd 条目，不支持 `user:group`；
- 不查询 NSS/LDAP，不解析附加组；
- 所有显式身份都会清空附加组；非 root 身份还会清空
  Inheritable/Effective/Permitted/Ambient Capabilities 并设置 `no_new_privs`；
- 显式 `0:0` 或名称 `root` 保留 root Capabilities，不设置 `no_new_privs`；
- 用户不存在或 passwd 非法时直接失败，不回退 root；
- `HOME`、`USER`、`LOGNAME` 由 `Env` 显式设置。

### `Expose`

- 转换为 AGS `CustomConfiguration.Ports` 的 TCP 端口；
- 整个 Spec 最多 8 个，不可重复；
- `49982`、`32000`、`57890` 为保留端口；
- 沙箱内 Loopback 通信不需要 Expose；
- 没有任何 Expose 时，Renderer 不发送 Ports 覆盖。

## Readiness Probe

```go
type ReadinessProbe struct {
    Path             string
    Port             int
    StartupTimeout   time.Duration
    Period           time.Duration
    Timeout          time.Duration
    FailureThreshold int
    SuccessThreshold int
}

probe := sandcamp.HTTPReadinessProbe("/healthz", 9200)
```

只支持从共享 Network Namespace 发起的 Loopback HTTP GET，HTTP 200–399 视为成功。

| 字段 | 默认值 | 作用 |
| --- | ---: | --- |
| `StartupTimeout` | 8 秒 | 首次 Ready 的最长等待时间 |
| `Period` | 500 毫秒 | 连续探测周期 |
| `Timeout` | 1 秒 | 单次 HTTP 探测上限 |
| `FailureThreshold` | 3 | Ready 后连续失败多少次变为 Unready |
| `SuccessThreshold` | 1 | Starting/Unready 后连续成功多少次变为 Ready |

首次 Ready 只负责启动 Gate；campd 在初始化完成后仍持续探测，因此 `/ready` 可以
200→503→200。Probe 失败只改变可观测就绪状态，不终止进程。

时间必须是整毫秒。`StartupTimeout` 范围 1–30 秒，`Period` 与 `Timeout` 范围
100 毫秒–30 秒。所有 Probe `StartupTimeout` 加所有 Job `Timeout` 的声明预算最多
25 秒，为 AGS 的 30 秒就绪期限保留启动开销。

Probe 的契约是“配置的共享 Loopback HTTP 端点可用”，而不是“声明该 Probe 的
Linux PID 已就绪”。监听或代理该端点的任意进程都可以满足 Probe；进程身份校验不在
Sandcamp Probe 的职责内。

## `RenderStart`

```go
func RenderStart(images ImageSet, spec Spec) (*ags.CustomConfiguration, error)
```

返回内容：

| 字段 | 内容 |
| --- | --- |
| `Command` | `/mnt/sandcamp/bin/campd` |
| `Args` | `['--']` |
| `Env` | Base64 编码的 runtime declaration v3 (`SANDCAMP_SPEC`) |
| `Ports` | 所有 Expose 端口；为空时省略 |
| `Probe` | AGS `GET /ready:49982` |

runtime declaration 是 Go SDK 与 campd 之间的序列化协议，也就是文档或代码中所说的
“wire”结构。调用方不应自行构造它。Renderer 为 Sidecar 注入解析后的 Image Volume
RootFS；`Expose` 已转换为 AGS Ports，所以不会继续出现在 wire Process 中。

顶层 `Args=['--']` 用于阻止主镜像原有 CMD 被 AGS 追加到 campd。它不是业务进程
参数。campd 启动 Sidecar 时会自行构造 sandrun argv，并在 sandrun 选项与
`Process.Command` 之间加入内部 `--`；调用方不处理这两个标记。

## 可识别错误

可以使用 `errors.Is`：

| 错误 | 含义 |
| --- | --- |
| `ErrInvalidImage` | 镜像引用、Registry Type 或 Sidecar Name 非法 |
| `ErrDuplicateSidecarImage` | Sidecar Image Name 重复 |
| `ErrMissingSidecarImage` | Sidecar Process 没有同名镜像 |
| `ErrInvalidSpec` | 名称、路径、Kind、命令、Env、User、Probe 或 Job 组合非法 |
| `ErrTooManyPorts` | Expose 超过 8 个 |
| `ErrDuplicatePort` | Expose 端口重复 |
| `ErrReservedPort` | 使用保留端口 |
| `ErrDuplicateProcess` | 进程名称重复 |
| `ErrStartupBudget` | Probe 与 Job 启动预算超过 25 秒 |
| `ErrSpecTooLarge` | 编码后的 `SANDCAMP_SPEC` 超过 120 KiB |

## 平台契约

- Runtime 镜像包含 `/bin/campd`、`/bin/campd.real` 和 `/bin/sandrun`；
- Runtime setuid launcher 以 PID 1 启动，并获得 EUID 0；
- campd/sandrun 具备 root 与 `CAP_SYS_ADMIN`；
- `/dev/vda` 是可用于 OverlayFS upper/work 的 ext4 设备；
- 主镜像与 Image Volume 架构一致；
- Main 与 Sidecar 共享 Network Namespace；
- Mount Namespace、OverlayFS 与 `pivot_root` 不是安全隔离边界。

完整请求示例见 [`examples/cookbook/main.go`](../examples/cookbook/main.go)。
