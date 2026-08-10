# campd

`campd` 是 Sandcamp 的最小化 Linux 启动管理器。

它从 `SANDCAMP_SPEC` 读取带版本号的 Base64 JSON 声明，依次启动 Sidecar，
最后启动主进程，并提供 `GET 0.0.0.0:49982/ready`。

启动探测只支持 Loopback HTTP GET。Campd 不提供 TCP 或 Exec 探测、进程重启
策略、cgroup 控制或观察面 API。

`campd` 不链接 `sandrun`，也不理解它的命令行选项；`RenderStart` 生成的启动声明只是
让 campd 把 `sandrun` 当作普通子进程执行。两个二进制保持独立职责，但内部命令行
必须版本兼容，并应在同一个 Runtime 镜像中一起构建、测试和发布。

主进程会继承 campd 从主镜像和 AGS 获得的环境，再应用自己的 `Process.Env`。
Sidecar 会先清空继承环境，只注入显式声明的 `Process.Env`，避免主容器凭证泄漏。

在仓库根目录构建静态 Linux/amd64 二进制：

```bash
RUSTFLAGS='-C linker=rust-lld' \
  cargo build -p campd --release --target x86_64-unknown-linux-musl
```
