# Image Volume 测试镜像

这些 Linux/amd64 镜像保留完整 OCI 根文件系统，用于本地验证 Image Volume。
每个镜像都可以导出后由独立的 `sandrun` 命令启动；sandrun 不依赖 campd，
也不提供 campd 内部 API。

Mount Namespace 和 `pivot_root` 能让进程从指定根文件系统解析可执行文件、
Dynamic Loader、动态库和绝对路径。它们只保证依赖正确，不是安全隔离边界；
Network/PID Namespace、cgroup 和文件描述符仍然继承自沙箱。省略 `--user` 时进程
身份和 Capabilities 也保持继承；指定命名用户时，sandrun 会显式降权并清空
Capabilities。

## Runtime Bundle

`runtime` 测试镜像打包了独立构建的静态 `/bin/campd` 和 `/bin/sandrun`。
将镜像挂载到 `/mnt/sandcamp` 后，可以通过 `/mnt/sandcamp/bin/campd` 和
`/mnt/sandcamp/bin/sandrun` 使用这两个二进制，无需修改主镜像。

campd 不依赖 sandrun 的 Crate，但会从结构化运行时声明构造 sandrun 命令行；测试
将两个二进制作为同一 Runtime 版本组合构建和验证。

## FastAPI Proxy

代理使用 Apache-2.0 协议的 `fastapi-proxy-lib` 0.3.0。除
`GET /healthz` 外，所有 HTTP/WebSocket 流量都会转发到 `UPSTREAM_URL`。
全栈验证会同时检查 HTTP 请求和 WebSocket Echo。

运行参数：

- `UPSTREAM_URL` 默认为 `http://127.0.0.1:8080/`
- `FASTAPI_HOST` 默认为 `0.0.0.0`
- `FASTAPI_PORT` 默认为 `9200`
- `LOG_LEVEL` 默认为 `info`

## OpenSandbox Egress

Egress 镜像固定使用官方 Linux/amd64 Manifest：
`sha256:0dd9727216b535fa34ef77495c3e465da848b888c28ee4be4f92c1003a97a71f`。

镜像保留上游 Entrypoint、网络工具、Python 和 Mitmproxy Runtime，而不是只复制
Go 二进制。

测试镜像标签沿用 `engress` 拼写，以兼容现有测试脚本。

## 只读约束

三个测试镜像都按只读挂载设计。sandrun 将 Image Volume 作为 OverlayFS
`lowerdir`，使用独立 writable layer 为普通镜像路径提供 Copy-on-Write，并为
`/tmp` 和 `/run` 覆盖可写 tmpfs。缺失的 `/etc/resolv.conf` 和
`/etc/hosts` 目标在 OverlayFS upper 中创建，再注入运行时生成的只读文件；
不再复制整棵 `/etc`。

`/proc`、`/dev` 和 `/sys` 使用独立挂载。共享或独立持久化目录仍通过
`sandrun --bind` 从调用方文件系统显式提供；普通写入只进入 OverlayFS upper，
sandrun 不会修改 Image Volume lower。
