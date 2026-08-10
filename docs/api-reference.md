# Sandcamp Go SDK API 参考

## 1. 安装

```bash
go get github.com/csjgg/sandcamp@<release-tag>
```

```go
import "github.com/csjgg/sandcamp"
```

Sandcamp 返回官方 AGS Go SDK `v20250920` 的 `StorageMount` 和
`CustomConfiguration` 类型，但不会发送云 API。

## 2. 核心调用

```go
mounts, err := sandcamp.RenderMounts(images)
if err != nil {
    return err
}
toolRequest.StorageMounts = append(toolRequest.StorageMounts, mounts...)

configuration, err := sandcamp.RenderStart(images, spec)
if err != nil {
    return err
}

toolConfiguration := *configuration
toolConfiguration.Image = &mainImage
toolConfiguration.ImageRegistryType = &registryType
toolRequest.CustomConfiguration = &toolConfiguration
startRequest.CustomConfiguration = configuration
```

两个函数都是确定性的纯函数：

- 不读取云凭证；
- 不访问网络或 Registry；
- 不创建 Tool 或 Instance；
- 不修改输入；
- 相同输入产生相同输出。

custom Tool 要求默认 `Command` 和 `Probe`，所以 Tool 与 Instance 都使用
`RenderStart` 输出；Tool 副本额外补充主镜像和 Resources。Instance 的
`MountOptions` 保持为空，直接继承 Tool 的 `StorageMounts`。

## 3. 镜像类型

### `Image`

```go
type Image struct {
    Reference         string
    ImageRegistryType string
}
```

用于声明 Sandcamp Runtime 镜像。

### `SidecarImage`

```go
type SidecarImage struct {
    Name              string
    Reference         string
    ImageRegistryType string
}
```

- `Name` 必须符合进程名称规则；
- `Name` 在一个 `ImageSet` 中不可重复；
- `Name` 必须与对应 `Process.Name` 一致；
- `Reference` 不允许为空、首尾空白或 NUL；
- `ImageRegistryType` 当前支持 `personal` 和 `enterprise`。

### `ImageSet`

```go
type ImageSet struct {
    SandcampRuntime Image
    Sidecars        []SidecarImage
}
```

`ImageSet` 只包含 Image Volume。主业务镜像继续配置在 AGS Tool 的
`CustomConfiguration.Image`。

## 4. `RenderMounts`

```go
func RenderMounts(images ImageSet) ([]*ags.StorageMount, error)
```

返回 Tool 创建阶段使用的只读 Image `StorageMounts`。SDK 拥有 Mount Name 和
MountPath：

| 镜像 | Name | MountPath |
| --- | --- | --- |
| Runtime | `sandcamp-runtime` | `/mnt/sandcamp` |
| Sidecar `NAME` | `sandcamp-sidecar-NAME` | `/mnt/sandcamp-sidecars/NAME` |

调用方应把结果追加到 `CreateSandboxToolRequest.StorageMounts`，不应解析或修改
内部名称和路径。

## 5. `Spec`

```go
type Spec struct {
    Sidecars []Process
    Main     Process
}
```

- Sidecar 按声明顺序启动并执行启动探测；
- Main 最后启动；
- 任一进程启动或探测失败，整个启动失败；
- 任一进程在就绪后退出，campd 会终止其余进程并退出；
- campd 不自动重启进程。

## 6. `Process`

```go
type Process struct {
    Name         string
    Command      []string
    Env          map[string]string
    WorkDir      string
    User         *ProcessUser
    Expose       []int
    StartupProbe *StartupProbe
}
```

### `Name`

- Sidecar 必填；Main 为空时默认 `main`；
- 长度 1–64；
- 首字符必须是字母或数字；
- 后续只能包含字母、数字、点、下划线和连字符；
- 同一个 `Spec` 内不可重复；
- Sidecar 必须存在同名 `SidecarImage`。

### `Command`

- 完整 argv，不是 Shell 字符串；
- `Command[0]` 是该进程镜像内部的规范化绝对路径；
- 不执行 Shell 展开、变量替换或引号解析；
- Sidecar 不需要也不允许由调用方拼接 sandrun 参数或 `--` 分隔符。

需要 Shell 时显式声明：

```go
Command: []string{"/bin/sh", "-c", "exec /app/server --port 8080"}
```

### `Env`

- Key 必须符合 `[A-Za-z_][A-Za-z0-9_]*`；
- Key 和 Value 不允许包含 NUL；
- 不允许覆盖 `SANDCAMP_SPEC`；
- Sidecar 只接收自己的 `Process.Env`；
- Main 保留主镜像和 AGS 环境，再由 `Process.Env` 覆盖；
- Image Volume 不会自动应用 OCI `ENV`，Sidecar 应显式补充 `PATH`、`HOME`
  和应用配置。

全部声明编码到单个 `SANDCAMP_SPEC`。产品上限为 120KiB；超过上限时返回
`ErrSpecTooLarge`。

120KiB 上限为 Linux 单个 argv/envp 字符串边界和平台启动开销预留空间。较大的
证书、策略或配置应通过文件挂载传递。

### `WorkDir`

- Main：主业务镜像内绝对路径；
- Sidecar：对应 Sidecar 镜像内绝对路径；
- SDK 自动将 Sidecar WorkDir 转换为内部 sandrun 参数；
- `/` 可作为 WorkDir。

### `User`

```go
// Main：显式数字身份
User: &sandcamp.ProcessUser{UID: 65532, GID: 65532}

// Sidecar：解析镜像内的命名用户
User: &sandcamp.ProcessUser{Name: "app"}
```

- Main 继续使用数字 UID/GID，由 campd 在 `exec` 前降权；
- Sidecar 只接受用户名，格式为 `[A-Za-z_][A-Za-z0-9_.-]{0,63}`；
- Sidecar 用户必须预先存在于镜像只读 lower RootFS 的 `/etc/passwd`，主 GID
  取该 passwd 条目；
- sandrun 先以 root 完成 OverlayFS、标准挂载、`pivot_root` 和 WorkDir，再清空
  Supplementary Groups 与 Capabilities，切换 UID/GID，启用 `no_new_privs`，
  最后 `exec`；
- `User=nil` 的 Sidecar 保持 root；
- 用户不存在、passwd 条目非法或重复时启动失败，绝不回退为 root；
- 不查询 NSS/LDAP，也不自动设置 `HOME`、`USER` 或 `LOGNAME`，这些环境变量仍由
  `Process.Env` 显式提供。

### `Expose`

- 写入 AGS `CustomConfiguration.Ports` 的 TCP 端口；
- 最多 8 个，不可重复；
- `49982`、`32000`、`57890` 是保留端口；
- 只声明外部访问端口，沙箱内部 Loopback 端口无需 Expose；
- 整个 `Spec` 无 Expose 时不发送 Ports 覆盖，保留 Tool 默认端口。

### `StartupProbe`

```go
StartupProbe: sandcamp.HTTPStartupProbe("/healthz", 9200)
```

只支持 Loopback HTTP GET 启动探测：

- 默认 ReadyTimeout：8 秒；
- 默认 Period：500 毫秒；
- 默认单次 Timeout：1 秒；
- 所有进程 ReadyTimeout 之和最多 25 秒；
- Path 必须是合法 HTTP origin-form；
- 仅用于启动，不是持续 Liveness Probe。

## 7. `RenderStart`

```go
func RenderStart(
    images ImageSet,
    spec Spec,
) (*ags.CustomConfiguration, error)
```

函数先校验 `ImageSet` 和 `Spec`，再确认每个 Sidecar Process 都存在同名镜像，
最后生成：

| 字段 | 内容 |
| --- | --- |
| `Command` | `/mnt/sandcamp/bin/campd` |
| `Args` | `["--"]` |
| `Env` | Base64 编码的 `SANDCAMP_SPEC` |
| `Ports` | 所有唯一 Expose 端口；为空时省略 |
| `Probe` | AGS `GET /ready:49982` |

Sidecar 的 rootfs、OverlayFS、workdir 和 sandrun 参数由 SDK 内部生成。
表中的 `Args=["--"]` 是 AGS 顶层配置标记，用于阻止主镜像原有 `CMD` 追加到
campd；它不是 Sidecar 命令。`RenderStart` 还会在内部 sandrun argv 中生成另一个
`--`，用于分隔 sandrun 选项与 `Process.Command`。调用方不需要设置任何一个。

调用方继续负责 Start 请求中的：

- `ToolId` 或 `ToolName`；
- `Timeout`；
- `ClientToken`；
- `AuthMode`；
- `Metadata`；
- 其他非 Sandcamp 配置。

`MountOptions` 不需要设置。

## 8. 可识别错误

调用方可以使用 `errors.Is`：

| 错误 | 含义 |
| --- | --- |
| `ErrInvalidImage` | 镜像引用、Registry Type 或 Sidecar Name 非法 |
| `ErrDuplicateSidecarImage` | Sidecar Image Name 重复 |
| `ErrMissingSidecarImage` | Process 没有同名 Sidecar Image |
| `ErrInvalidSpec` | 名称、路径、命令、Env、User 或 Probe 非法 |
| `ErrTooManyPorts` | Expose 超过 8 个 |
| `ErrDuplicatePort` | 端口重复 |
| `ErrReservedPort` | 使用 Sandcamp/AGS 保留端口 |
| `ErrDuplicateProcess` | 进程名称重复 |
| `ErrStartupBudget` | 串行启动探测预算超过 25 秒 |
| `ErrSpecTooLarge` | `SANDCAMP_SPEC` 编码后超过 120KiB |

```go
configuration, err := sandcamp.RenderStart(images, spec)
switch {
case errors.Is(err, sandcamp.ErrMissingSidecarImage):
    return fmt.Errorf("Sidecar 镜像声明不完整: %w", err)
case errors.Is(err, sandcamp.ErrSpecTooLarge):
    return fmt.Errorf("Sidecar 环境变量过多: %w", err)
case err != nil:
    return err
default:
    request.CustomConfiguration = configuration
}
```

## 9. 平台契约

- Linux/amd64，主镜像和 Image Volume 架构一致；
- Runtime 镜像包含 `/bin/campd`、`/bin/campd.real` 和 `/bin/sandrun`；
- Runtime setuid launcher 能以 PID 1 启动；
- campd/sandrun 具备 root 与 `CAP_SYS_ADMIN`；
- 启用 Egress 时具备 `CAP_NET_ADMIN`；
- `/dev/vda` 可作为 OverlayFS upper/work 介质；
- 主进程与 Sidecar 共享 Network Namespace；
- Sandcamp 的 mount namespace/chroot 不是安全隔离边界。

完整接入示例见 [`Cookbook`](../examples/cookbook/README.md)，运行原理见
[`技术方案`](design.md)。
