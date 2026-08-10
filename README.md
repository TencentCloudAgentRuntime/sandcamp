# Sandcamp

Sandcamp 是一个专注于进程启动兼容性的 AGS SDK。它无需修改主业务镜像，即可在
同一个 Linux 沙箱中启动主进程，以及通过 Image Volume 挂载的 Sidecar。

[使用示例](examples/cookbook/README.md) ·
[API 参考](docs/api-reference.md) ·
[技术方案](docs/design.md) ·
[排障手册](docs/troubleshooting.md) ·
[验证范围](docs/validation.md)

## 组件

| 组件 | 职责 |
| --- | --- |
| Go SDK | 生成 Image `StorageMounts`，校验进程声明并生成 AGS `CustomConfiguration` |
| `campd` | 作为 PID 1 按顺序启动进程、聚合启动探针并转发信号 |
| `sandrun` | 在独立 Mount Namespace 和 OverlayFS 根文件系统中启动 Sidecar |
| `campd-launcher` | 受限的 setuid Bootstrap，只允许 PID 1 启动固定的 `campd.real` |

Sandcamp 不替代官方 AGS SDK。Tool/Instance 生命周期、鉴权、连接、重试和镜像元数据
解析仍由调用方负责。

## 启动链路

```text
AGS PID 1
  └─ campd-launcher
       └─ campd (root)
            ├─ sandrun → Sidecar A RootFS → exec Sidecar A
            ├─ sandrun → Sidecar B RootFS → exec Sidecar B
            └─ exec Main

GET 127.0.0.1:49982/ready → AGS readiness
```

主镜像不需要包含 campd 或 sandrun。二者一起打包在 Sandcamp Runtime Image
Volume 中，但保持独立二进制和进程职责，并作为同一版本组合进行验证。

Sandrun 只创建 Mount Namespace，不创建 PID、Network、User 或 IPC Namespace。
主进程与 Sidecar 共享网络、PID 可见性、cgroup 和沙箱资源限制；该机制用于依赖
兼容，不构成额外的安全隔离边界。

## 快速开始

```bash
go get github.com/csjgg/sandcamp@latest
```

```go
import "github.com/csjgg/sandcamp"

mounts, err := sandcamp.RenderMounts(images)
if err != nil {
    return err
}
configuration, err := sandcamp.RenderStart(images, processes)
if err != nil {
    return err
}

toolRequest.StorageMounts = append(toolRequest.StorageMounts, mounts...)

toolConfiguration := *configuration
toolConfiguration.Image = &mainImage
toolConfiguration.ImageRegistryType = &registryType
toolRequest.CustomConfiguration = &toolConfiguration

startRequest.CustomConfiguration = configuration
// MountOptions 保持 nil，继承 Tool.StorageMounts。
```

AGS custom Tool 要求默认 `Command` 和 `Probe`，因此 Tool 和 Instance 都应使用
`RenderStart` 的结果。完整的镜像、进程和请求组合见
[`examples/cookbook`](examples/cookbook/README.md)。

## 根文件系统与用户

Sidecar Image Volume 作为 immutable OverlayFS lowerdir。sandrun 使用 AGS 提供的
ext4 设备承载独立的 upperdir/workdir，挂载完成后执行 `pivot_root`，因此普通镜像
路径具有 Copy-on-Write 语义，而 lowerdir 保持不变。

Main 使用数字身份：

```go
User: &sandcamp.ProcessUser{UID: 65532, GID: 65532}
```

Sidecar 可以使用镜像内用户名：

```go
User: &sandcamp.ProcessUser{Name: "app"}
```

命名用户必须存在于 Sidecar lower RootFS 的 `/etc/passwd`。sandrun 在全部挂载和
WorkDir 切换完成后清空附加组与 Capabilities、设置 `no_new_privs`，再切换到该
用户的 UID 和主 GID。用户缺失或 passwd 非法时直接失败，不回退 root。

Image Volume 不会自动应用 Sidecar OCI Config。`Entrypoint`、`Cmd`、`Env`、
`WorkingDir`、`User` 和健康检查需要显式转换为 `Process` 声明。

## 运行约束

- 主镜像和 Image Volume 必须是 Linux，并使用相同 CPU 架构。
- campd 和 sandrun 的挂载流程需要 root 与 `CAP_SYS_ADMIN`。
- 修改共享网络策略的 Sidecar 还需要相应网络 Capability。
- Sidecar `overlay-id` 在同一个沙箱内必须唯一。
- 全部进程声明编码到单个 `SANDCAMP_SPEC`，SDK 产品上限为 120KiB。
- 启动探针只支持 Loopback HTTP GET，并按声明顺序执行。
- campd 不提供进程重启或持续健康检查。

## 仓库结构

```text
.
├── cmd/
│   ├── campd-launcher/        # 受限 setuid Bootstrap
│   ├── campd/                 # PID 1 启动管理器
│   └── sandrun/               # Sidecar RootFS 启动器
├── docs/                      # API、设计、排障和验证范围
├── examples/cookbook/         # 可运行的 Go 示例
├── images/runtime/            # Runtime Image Volume 镜像
├── scripts/                   # Linux/Docker 集成测试
├── test/                      # 测试镜像和协议夹具
├── images.go                  # Image Volume 挂载生成器
├── render.go                  # AGS 启动配置生成器
└── spec.go                    # 进程声明 API
```

## 开发

```bash
task fmt
task test
task lint
task build
task test:sandrun-linux
task test:stack-linux
```

不使用 Task 时可以直接执行：

```bash
go test ./...
cargo test --workspace
go run ./examples/cookbook inspect
```
