# 验证范围

本文记录当前实现的测试范围和仍需补充的验证项。环境相关的 Tool/Instance ID、镜像
仓库地址和云端原始响应不提交到仓库。

## 自动化测试

### Go SDK

```bash
go test ./...
```

覆盖：

- `ImageSet` 和镜像引用校验；
- Runtime/Sidecar `StorageMounts` 的稳定名称、路径和只读属性；
- `Spec`、进程名称、命令、环境变量、WorkDir、端口和探针校验；
- Main 数字 UID/GID 与 Sidecar 命名用户转换；
- Tool 默认 `CustomConfiguration` 与 Instance 覆盖配置组合；
- `SANDCAMP_SPEC` 编码、大小限制和输入不可变性。

### Rust

```bash
cargo test --workspace
cargo fmt --all --check
RUSTFLAGS='-C linker=rust-lld' \
  cargo clippy --workspace --all-targets \
  --target x86_64-unknown-linux-musl -- -D warnings
```

覆盖：

- campd 声明解析、启动顺序和就绪状态；
- 进程退出与信号转发；
- sandrun 参数、挂载目标和用户名校验；
- 非法 Payload、重复进程和探针预算拒绝。

## Linux 集成测试

构建静态 Linux/amd64 二进制和测试镜像后执行：

```bash
task test:sandrun-linux
task test:stack-linux
```

`test-sandrun-linux.sh` 覆盖：

- 只读 OCI lower RootFS 与通用 OverlayFS Copy-on-Write；
- upper 根目录继承 lower `/` 的 UID、GID 和 Mode；
- writable/readonly Bind、tmpfs 和标准挂载；
- `pivot_root` 后的 WorkDir；
- glibc、musl 和静态可执行文件；
- Overlay Identity 锁和重启复用；
- 命名用户 UID/GID、空附加组、零 Capabilities 与 `NoNewPrivs=1`；
- 用户缺失时失败且不回退 root；
- lowerdir 不被修改；
- `exec` 后的信号传递。

`test-stack-linux.sh` 覆盖：

- campd、主进程、FastAPI 和 Egress 的完整启动顺序；
- root 与非 root Main；
- HTTP/WebSocket 代理；
- 共享 Network Namespace 中的 Egress 规则；
- 主进程与 Sidecar 环境变量隔离；
- 共享目录和只读配置文件。

## AGS 集成契约

当前实现已按以下契约完成环境验证：

- Runtime、FastAPI 和 Egress 可通过只读 Image Volume 挂载；
- setuid launcher 可以从非 root 主镜像启动 root campd；
- campd 完成 Bootstrap 后可将 Main 降权到数字 UID/GID；
- root Sidecar 与镜像 `/etc/passwd` 命名用户 Sidecar 可以同时启动；
- Sidecar 在挂载完成后清空附加组和 Capabilities，并设置 `no_new_privs`；
- AGS 通过 campd `GET :49982/ready` 聚合全部启动探针；
- Instance 继承 Tool `StorageMounts`，无需实例级 `MountOptions`；
- Sidecar OverlayFS 写层位于 AGS 提供的 ext4 设备，Image Volume lower 保持只读；
- 大型 `SANDCAMP_SPEC` 在 120KiB 产品上限内能够完成编码和传递。

云端验证记录应保存在受控环境中，并在共享前移除凭证、账号、Region、Tool ID、
Instance ID、镜像仓库命名空间和完整请求响应。

## 待补充验证

- 不暴露端口时，Tool 默认端口与 Instance 覆盖的完整行为；
- Runtime 加多个 Sidecar 时的 Image Volume 数量和容量限制；
- Pause/Resume 与 Snapshot/Restore 后 Mount Namespace 和 Overlay 写层行为；
- campd 在就绪前退出时的平台状态与日志可观测性；
- 镜像挂载、OverlayFS 和串行探针的冷启动开销；
- Stop 与 Timeout 下的优雅退出；
- 多个并发 Instance 的资源和状态隔离。

## 不在 Sandcamp 验证范围内

- Registry/Hub 镜像元数据查询；
- AGS 鉴权、API 重试和控制面生命周期语义；
- Pause、Resume、Connect 等官方 SDK 封装；
- Image Volume 下载和解包；
- 平台磁盘容量、配额和 Instance 删除后的回收策略。
