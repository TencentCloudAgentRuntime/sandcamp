# 验证范围

本文记录当前自动化覆盖与明确接受的运行边界。普通单元测试不访问云 API，也不会创建
AGS 资源。

GitHub Actions 在 Pull Request、`main` 更新和手动触发时执行格式、Go/Rust 测试、
Clippy、静态 Linux 构建与 Docker 整栈回归。当前 CI 不创建 Release，也不推送镜像。

## Go SDK

```bash
go test ./...
go vet ./...
```

覆盖：

- `ImageSet`、镜像引用、Registry Type 和稳定 Mount Name/Path；
- Sidecar-only、单/多 Main、Sidecars→Main 顺序；
- Service 默认 Kind、RunToCompletion Job 与非法生命周期组合；
- Readiness 默认值、阈值、持续时间和 25 秒启动预算；
- Main 数字用户、Sidecar 命名用户与错误组合；
- Command、Env、WorkDir、Expose、保留端口和 120 KiB 编码上限；
- runtime declaration v2 的 Main/Sidecar 类型分离；
- Sidecar RootFS 注入、Main 隐式 RootFS 和 AGS Ports 转换；
- Tool 默认配置与 Instance 配置组合；
- E2E 49 个场景的 Spec 全量渲染。

## Rust

```bash
cargo test --workspace
cargo fmt --all --check
RUSTFLAGS='-C linker=rust-lld' \
  cargo clippy --workspace --all-targets \
  --target x86_64-unknown-linux-musl -- -D warnings
```

campd/sandrun 单元测试覆盖：

- runtime declaration v2 严格反序列化与旧版本拒绝；
- Main/Sidecar 字段、Kind、用户、Bind、路径、环境和预算校验；
- campd 构造 sandrun argv；
- Readiness 成功/失败阈值与恢复；
- sandrun 参数、Bind/tmpfs 目标、WorkDir 和用户名校验。

Linux 上额外执行 5 个 campd 进程集成测试：

- Main 继承环境、Sidecar 环境隔离、`SANDCAMP_SPEC` 不泄漏；
- 无 Probe Service 退出后 `/ready=503`，健康 Peer 继续运行；
- Probe `200→503→200`，且 HTTP 成功不能掩盖 PID 已退出；
- Job 成功 Gate、非零失败阻止后续 Service；
- Job 超时后 TERM→KILL 清理进程组，campd 保持存活且未就绪；
- 外部 SIGTERM 转发与 campd 退出码。

## Linux RootFS 与整栈回归

```bash
task test:campd-linux
task test:sandrun-linux
task test:stack-linux
```

`test-sandrun-linux.sh` 覆盖：

- immutable OCI lower 与 OverlayFS Copy-on-Write；
- upper 根目录继承 lower `/` 的 UID、GID 和 Mode；
- writable/readonly Bind、tmpfs 与标准挂载；
- `pivot_root` 后 WorkDir；
- glibc、musl 与静态二进制；
- Overlay Identity 锁、并发拒绝和重启复用；
- 命名用户 UID/GID、空附加组、四组零 Capability、`NoNewPrivs=1`；
- 用户缺失严格失败；
- lower 不变、exec 信号链路、FastAPI 与 Egress 完整 RootFS。

`test-stack-linux.sh` 分别以 root 和 `65532:65532` 主镜像入口执行：

- setuid launcher、PID 1 限制和 Runtime 文件模式；
- campd→sandrun→FastAPI/Egress→Main 完整顺序；
- Main 数字用户降权和 `no_new_privs`；
- HTTP/WebSocket、共享 Loopback 和 Egress allow/deny；
- Main/Sidecar 环境隔离；
- Overlay 写入、immutable lower、共享读写目录和只读配置；
- 外部 SIGTERM 下的进程组关停。

## AGS 运行态回归

[`test/e2e`](../test/e2e/README.md) 是显式运行的云端测试。当前目录包含 49 个场景：

| 范围 | 数量 | 主要内容 |
| --- | ---: | --- |
| 启动、组合与 Readiness | 9 | Sidecar-only、多 Main、两类 Init Job、无 Probe Service、持续降级/恢复 |
| 身份与权限 | 3 | Main 数字用户、Alpine/glibc Sidecar 命名用户 |
| 文件系统、进程与网络 | 10 | Overlay、argv/env/workdir、双向 HTTP、UDP、公网、Fanout、子进程拓扑、Netfilter 写入 |
| 生命周期与边界 | 6 | Main/Sidecar 独立退出、同组清理、TERM→KILL、setsid、共享 Probe 端点 |
| 真实镜像、网络策略与额外挂载 | 14 | FastAPI、Nginx Main、Egress 多策略与规则生命周期、envd Main Service |
| 预期拒绝 | 7 | 用户/命令缺失、Probe 超时、启动崩溃、Job 非零与超时 |

其中 3 个 Egress 负向场景归入网络策略范围，分别验证非法策略、控制端口冲突和
non-root 权限不足，因此表内各行与场景总数不存在重复计数。

测试专用 Runtime 额外包含 root Observer，通过 `/proc`、主动 HTTP/HTTPS/UDP 请求和
启动前/运行中 Netfilter 快照收集有界证据；
正式 Runtime 不包含该二进制。证据只保留白名单环境变量，不记录凭证或 Observer
Token。

## 接受的运行边界

完整云端矩阵保留两个 `boundary` 场景，用证据记录边界，但不会把符合公开语义的行为
记为失败：

- `setsid-process-group-boundary`：记录新 Session 后代是否仍存活；campd 不承诺跨
  Session 回收；
- `shared-probe-endpoint`：确认 Probe 判断配置的共享 HTTP 端点，不校验响应者 PID。

外部关停时 campd 会向全部存活进程组转发信号，但不保证 Main 与 Sidecar 的信号先后
顺序。当前 API 不承诺该顺序。

## 待补充

- Pause/Resume 与 Snapshot/Restore 后的 Mount Namespace 和 Overlay 写层行为；
- 多个并发 Instance 的磁盘、锁和状态隔离；
- Runtime 与大量 Image Volume 的容量和冷启动边界；
- AGS Stop、Timeout 与平台 Probe 策略的长期稳定性矩阵。

云端证据共享前应移除凭证、账号、Region、Tool ID、Instance ID、镜像仓库命名空间和
完整请求响应。
