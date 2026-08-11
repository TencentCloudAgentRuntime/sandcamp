# campd

`campd` 是 Sandcamp 的最小化 Linux 启动管理器。

它从 `SANDCAMP_SPEC` 读取带版本号的 Base64 JSON 声明，先按顺序处理 Sidecar，
再按顺序处理零到多个 Main，并提供 `GET 0.0.0.0:49982/ready`。Service 可以持续
运行，RunToCompletion Job 必须成功退出后才会处理下一项。

Readiness 只支持共享 Loopback 上的 HTTP GET。首次成功负责启动 Gate，之后持续
影响聚合 `/ready`；它判断配置的端点，不校验响应者 PID。campd 不提供 TCP 或 Exec
探测、进程重启策略、cgroup 控制或观察面 API。

`campd` 不链接 `sandrun` 的 Crate，但会根据结构化 Sidecar 声明构造 `sandrun`
命令行。Go SDK 不暴露这层内部参数。两个二进制保持独立职责，但命令行协议必须版本
兼容，因此应在同一个 Runtime 镜像中一起构建、测试和发布。

Main 进程会继承 campd 从主镜像和 AGS 获得的环境，再应用自己的 `Process.Env`。
Sidecar 会先清空继承环境，只注入显式声明的 `Process.Env`，避免主容器凭证泄漏。

Main 和 Sidecar 都支持镜像用户名或显式数字 UID/GID。Main 名称由 campd 在任何
声明启动前从主镜像 `/etc/passwd` 解析并缓存；Sidecar 名称交给 sandrun 从自己的
immutable lower RootFS 解析。非 root 身份会清空附加组与 Capabilities，并启用
`no_new_privs`；显式 root 保留 root Capabilities。

在仓库根目录构建静态 Linux/amd64 二进制：

```bash
RUSTFLAGS='-C linker=rust-lld' \
  cargo build -p campd --release --target x86_64-unknown-linux-musl
```
