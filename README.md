# Sandcamp

Sandcamp 是一个用于 AGS 的进程编排 SDK。它让一个沙箱同时运行：

- 主镜像中的零到多个进程；
- 通过 Image Volume 挂载的 Sidecar 镜像进程；
- 在 Service 之前执行并成功退出的 Init Job。

主镜像不需要包含 `campd` 或 `sandrun`。这两个静态二进制由单独的 Runtime Image
Volume 提供。

组织仓库首次推送 `v*-beta.*` Git tag（例如 `v0.1.0-beta.1`）后，CI 会先执行完整
回归，再把 `linux/amd64` 预发布构建产物保存为
`ghcr.io/tencentcloudagentruntime/sandcamp-runtime:beta` 和对应版本标签。普通 `main`
push 与 Pull Request 只执行测试，不发布镜像。AGS Image Volume 当前不能直接从 GHCR
拉取；使用前必须把该 Runtime 镜像同步到调用方自己的腾讯云 CCR 或 TCR。需要固定
内容时，优先同步版本标签、`sha-<完整 Git Commit>` 或 Digest。

[完整 Cookbook](examples/cookbook/main.go) ·
[API 参考](docs/api-reference.md) ·
[技术方案](docs/design.md) ·
[排障手册](docs/troubleshooting.md) ·
[验证范围](docs/validation.md) ·
[运行态回归](test/e2e/README.md)

## 最小接入

首个版本发布前，可以直接依赖确定的 Git Commit：

```bash
go get github.com/TencentCloudAgentRuntime/sandcamp@<commit>
```

先把 Runtime 镜像同步到 AGS 能访问的 Registry。例如使用 CCR：

```bash
docker pull ghcr.io/tencentcloudagentruntime/sandcamp-runtime:beta
docker tag \
  ghcr.io/tencentcloudagentruntime/sandcamp-runtime:beta \
  ccr.ccs.tencentyun.com/<namespace>/sandcamp-runtime:beta
docker push ccr.ccs.tencentyun.com/<namespace>/sandcamp-runtime:beta
```

TCR 使用对应实例的 Registry 地址。主镜像与 Sidecar 镜像同样需要位于 AGS 支持的
Registry。Sandcamp 对 AGS 的两种 Image Registry 类型提供了明确常量：

| SDK 常量 | AGS 值 | Registry |
| --- | --- | --- |
| `sandcamp.ImageRegistryPersonal` | `personal` | 腾讯云 CCR 个人版 |
| `sandcamp.ImageRegistryEnterprise` | `enterprise` | 腾讯云 TCR 企业版 |

GHCR 仅作为构建产物的分发来源，不能填写到 `Image.Reference` 后再标记为 `personal`
或 `enterprise`。

先声明 Runtime、Sidecar 镜像和进程：

```go
registryType := sandcamp.ImageRegistryPersonal // 企业版使用 ImageRegistryEnterprise

images := sandcamp.ImageSet{
    SandcampRuntime: sandcamp.Image{
        Reference:         runtimeImage,
        ImageRegistryType: registryType,
    },
    Sidecars: []sandcamp.SidecarImage{{
        Name:              "proxy",
        Reference:         proxyImage,
        ImageRegistryType: registryType,
    }},
}

spec := sandcamp.Spec{
    Sidecars: []sandcamp.Process{{
        Name:    "proxy", // 与 SidecarImage.Name 对应
        Command: []string{"/usr/local/bin/python", "/opt/proxy/app.py"},
        WorkDir: "/opt/proxy",
        User:    &sandcamp.ProcessUser{Name: "app"},
        Env: map[string]string{
            "HOME": "/home/app",
            "PATH": "/usr/local/bin:/usr/bin:/bin",
        },
        Expose: []int{9200},
        Probe:  sandcamp.HTTPReadinessProbe("/healthz", 9200),
    }},
    Main: []sandcamp.Process{
        {
            Name:    "prepare",
            Kind:    sandcamp.RunToCompletion,
            Command: []string{"/app/prepare"},
            Timeout: 2 * time.Second,
        },
        {
            Name:    "api",
            Command: []string{"/app/server"},
            User:    &sandcamp.ProcessUser{UID: 0, GID: 0},
            Expose:  []int{8080},
            Probe:   sandcamp.HTTPReadinessProbe("/healthz", 8080),
        },
    },
}
```

`Sidecars` 先按声明顺序处理，随后处理 `Main`。`Main` 是数组，因此可以为空、包含
一个进程，或让同一主镜像 RootFS 启动多个进程。

再把声明转换为 AGS 请求字段：

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

startRequest.CustomConfiguration = configuration
// MountOptions 保持 nil，Instance 继承 Tool.StorageMounts。
```

AGS custom Tool 要求默认 `Command` 和 `Probe`，所以 Tool 与 Instance 都使用
`RenderStart` 的结果。Tool 的副本再补充主镜像和资源配置。完整、直接发送请求的
代码见 [`examples/cookbook/main.go`](examples/cookbook/main.go)。

运行 Cookbook 时从示例配置创建本地 `.env`：

```bash
cp examples/cookbook/.env.example examples/cookbook/.env
# 编辑 examples/cookbook/.env
go -C examples/cookbook run .
```

Cookbook 使用独立 `go.mod` 管理示例依赖。真实 `.env` 已被 Git 忽略；进程已有的环境
变量优先于文件中的同名值。

## 运行原理

```text
AGS PID 1
  └─ campd-launcher (受限 setuid bootstrap)
       └─ campd (root)
            ├─ sandrun → OverlayFS / pivot_root → Sidecar Service 或 Job
            ├─ sandrun → OverlayFS / pivot_root → Sidecar Service 或 Job
            ├─ exec Main Job
            └─ exec Main Service(s)

GET 127.0.0.1:49982/ready → 聚合 Readiness
```

Go SDK 保留结构化声明，并把 Sidecar 名称解析为对应 Image Volume 的挂载路径。
`campd` 在运行时构造 `sandrun` 参数；调用方只填写进程镜像中的 `Command`、
`WorkDir` 和 `User`，不拼接 `sandrun` 或 `--`。

`sandrun` 只为 Sidecar 创建 Mount Namespace。它不创建 PID、Network、User 或 IPC
Namespace，因此 Main 与 Sidecar 共享 Loopback、PID 可见性、cgroup 和沙箱资源限制。
Mount Namespace 与 `pivot_root` 用于依赖兼容，不构成额外的安全隔离边界。

## 进程与就绪语义

`Process.Kind` 省略时默认为 `Service`：

- Service 有 `Probe`：首次达到成功阈值后才启动下一项，之后持续探测；
- Service 没有 `Probe`：进程成功创建即通过启动 Gate，存活 PID 代表 Ready；
- `RunToCompletion`：必须在 `Timeout` 内以 0 退出，成功后才处理下一项；
- 声明中至少要有一个 Service，不能只有 Job。

campd 的 `/ready` 是动态聚合状态。任一 Service 退出或探针达到失败阈值会返回 503；
探针恢复到成功阈值后可再次返回 200。campd 不自动重启进程，也不会因一个 Service
退出而终止其他 Service。退出进程遗留在同一进程组的子进程会先收到 SIGTERM，超时
后升级为 SIGKILL。

如果 Init Job 非零退出、超时，或某个 Service 在初始化阶段失败，后续声明不会启动，
已经启动的 Service 保持运行，但整体 `/ready` 保持 503。

## RootFS 与用户

Sidecar Image Volume 是 immutable OverlayFS lowerdir。`sandrun` 使用 AGS 提供的 ext4
设备承载独立 upperdir/workdir，完成挂载后执行 `pivot_root`；普通镜像路径具有
Copy-on-Write 语义，而 lowerdir 不变。

Main 和 Sidecar 使用同一个身份模型。可以直接指定数字 UID/GID：

```go
User: &sandcamp.ProcessUser{UID: 65532, GID: 65532}
```

也可以使用进程所属镜像内的用户名：

```go
User: &sandcamp.ProcessUser{Name: "app"}
```

Main 名称从主镜像 `/etc/passwd` 解析；Sidecar 名称从对应 Sidecar immutable lower
RootFS 的 `/etc/passwd` 解析。同一个名称可以在不同镜像中得到不同 UID/GID。数字
身份不查询 passwd，直接应用声明的 UID/GID。

`User=nil` 时继承运行时身份，通常为 root。显式选择非 root 身份时会清空附加组与
Capabilities，并设置 `no_new_privs`；显式选择 `0:0` 或名称 `root` 时保留 root
Capabilities。用户缺失或 passwd 非法时直接失败，不回退 root。Sandcamp 不查询
NSS/LDAP、解析附加组，也不自动设置 `HOME`、`USER` 或 `LOGNAME`。

Image Volume 不会自动应用 OCI `Entrypoint`、`Cmd`、`Env`、`WorkingDir`、`User` 或
健康检查；Sidecar 需要显式声明这些运行信息。

## 当前边界

- 主镜像与所有 Image Volume 必须是兼容的 Linux CPU 架构；
- OverlayFS 与 Mount Namespace 需要 root 和 `CAP_SYS_ADMIN`；
- 修改共享网络策略的 Sidecar 还需要相应网络 Capability；
- 全部声明编码到一个 `SANDCAMP_SPEC`，编码后上限为 120 KiB；
- 启动预算是所有 Probe `StartupTimeout` 与 Job `Timeout` 之和，最多 25 秒；
- HTTP Probe 表示共享 Loopback 上的端点状态，不承诺响应者属于某个 Linux PID；
- campd 的后代回收保证以进程组为边界，主动 `setsid` 的后代不在该保证内；
- Sandcamp 不提供单进程重启、cgroup 分配或 Registry 元数据解析。

## 开发与验证

```bash
task fmt
task test
task lint
task build
task test:campd-linux
task test:sandrun-linux
task test:stack-linux
task e2e:list
task e2e:build
```

普通单元测试不会访问云 API。真实 AGS 回归需要显式运行
[`test/e2e`](test/e2e/README.md)。Cookbook 会实际创建 Tool 和 Instance，也需要在
准备好镜像与凭证后单独执行。
