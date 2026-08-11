# AGS 运行态回归

这套测试把 Sandcamp 生成的配置放进真实 AGS Instance，验证实际进程、RootFS、
Namespace、网络和生命周期行为。它是显式运行的云端测试；普通 `go test ./...`
不会访问云 API，也不会创建资源。

Runner 为一次测试创建一个 Tool，并按顺序为每个场景创建独立 Instance。这样可以
复用同一组 Image Volume 挂载，又不会在场景之间复用 Overlay 写层或进程状态。
每个 Instance 结束后立即删除，最后删除 Tool；即使场景失败也执行清理。

## 观察方式

测试 Agent 通过 `/proc` 和夹具事件收集以下有界证据：

- 真实 UID、GID、附加组、Capabilities 和 `NoNewPrivs`；
- PID、Network、Mount 等 Namespace 链接；
- argv、工作目录、RootFS 文件系统类型和指定文件；
- 进程启动、探针变化、信号和退出时间线；
- 由 Observer 主动发起的 HTTP、HTTPS、UDP 和 Egress 策略请求；
- 启动前与运行中的 `iptables-save`、`nft list ruleset` 有界快照。

Alpine 测试 Agent 包含 `iptables` 和 `nft`，用于观察共享 Network Namespace；正式
Runtime 和正式 Sidecar 镜像不包含这些诊断工具。对于 Nginx 这类会重写进程环境的
真实守护进程，Observer 只按场景声明的绝对可执行文件路径补充跟踪，不做模糊进程名
匹配。

普通场景把 root Observer 作为第一个 Sidecar。需要让受管进程退出的生命周期场景，
先由测试 Runtime 启动一个不受 campd 管理的 root Observer，再把 PID 1 替换为
campd。这样可以在目标 Service 退出后继续观察 Peer、`/ready` 和进程组清理；持续
Readiness 场景则可以直接使用受管 Observer。

[`test/e2e/images/runtime/Dockerfile`](images/runtime/Dockerfile) 是专用测试 Runtime，
其中额外包含 `sandcamp-e2e-agent`。正式 Runtime 继续使用
[`images/runtime/Dockerfile`](../../images/runtime/Dockerfile)，不包含测试探针。

证据只保留测试白名单环境变量，不记录云凭证、Observer Token 或任意业务环境。
响应正文和事件数量也有上限。

## 场景

当前共有 49 个场景：

| 范围 | 数量 | 主要内容 |
| --- | ---: | --- |
| 启动、组合与 Readiness | 9 | Sidecar-only、多 Main、Main/Sidecar Init Job、无 Probe Service、持续降级/恢复 |
| 身份与权限 | 3 | root、数字 Main、Alpine/glibc 命名用户 |
| 文件系统、进程与网络 | 10 | Overlay、argv/env/workdir、HTTP 双向 Loopback、UDP、公网、Fanout、子进程拓扑、Netfilter 写入 |
| 生命周期与边界 | 6 | Main/Sidecar 独立退出、同组清理、TERM→KILL、`setsid`、共享 Probe 端点 |
| 真实镜像、网络策略与额外挂载 | 14 | FastAPI、Nginx Main 反向代理、Egress HTTP/HTTPS/通配符/默认策略/规则安装与清理、envd |
| 非法启动 | 7 | 缺失用户/命令、Probe 超时、启动崩溃、Job 非零与超时 |

另外 3 个 Egress 负向场景归入网络策略：非法 JSON、控制端口冲突和 non-root 启动均
必须在进入 RUNNING 前失败。

用下面的命令查看逐项说明：

```bash
go run ./test/e2e/cmd/runner list
```

`boundary` 场景记录两项有意接受的语义：进程组回收不覆盖主动 `setsid` 的后代，
Probe 只判断共享 Loopback 端点而不校验响应者 PID。这些场景保留运行态证据，但符合
该语义时正常通过。故意构造的非法初始化如果在进入 RUNNING 前以已知的 ContainerStart、
ContainerProbe 或就绪 Timeout 失败，状态为 `EXPECTED_REJECT`；鉴权、内部错误等无关
错误仍记为 `FAIL`。

## 准备镜像

```bash
task e2e:build
```

该命令生成本地 Linux/amd64 镜像：

```text
sandcamp-e2e-agent:alpine-local
sandcamp-e2e-agent:glibc-local
sandcamp-runtime:e2e-local
sandcamp-e2e-nginx:local
```

将它们推送到测试环境可访问的镜像仓库后，配置以下环境变量：

```text
TENCENTCLOUD_SECRET_ID
TENCENTCLOUD_SECRET_KEY
TENCENTCLOUD_REGION           # 可选，默认 ap-guangzhou
AGS_ROLE_ARN

SANDCAMP_E2E_RUNTIME_IMAGE
SANDCAMP_E2E_AGENT_IMAGE
SANDCAMP_E2E_GLIBC_IMAGE      # 可选，默认复用 Alpine Agent
SANDCAMP_E2E_MAIN_IMAGE       # 通常与 Alpine Agent 相同
SANDCAMP_E2E_FASTAPI_IMAGE    # 运行全部场景时需要
SANDCAMP_E2E_EGRESS_IMAGE     # 运行全部场景时需要
SANDCAMP_E2E_ENVD_IMAGE       # 运行全部场景时需要
SANDCAMP_E2E_NGINX_IMAGE      # 运行全部场景时需要，作为该场景的 Main 镜像
SANDCAMP_E2E_REGISTRY_TYPE    # 可选，默认 personal
```

Runner 为完整矩阵生成 10 个 Storage Mount，包含 Runtime、夹具、真实 Sidecar 和
envd 单文件挂载；Nginx 通过 Instance 的 Main Image 覆盖运行，不额外占用 Mount。
不要再向该 Tool 追加测试无关的 Mount。

## 运行

先做只读鉴权检查，再跑最小场景：

```bash
go run ./test/e2e/cmd/runner doctor
go run ./test/e2e/cmd/runner run --scenario=minimal-root
```

也可以在一个 Tool 下按给定顺序运行若干场景：

```bash
go run ./test/e2e/cmd/runner run \
  --scenario=sidecar-only,multiple-main-services,main-init-job
```

运行完整矩阵：

```bash
go run ./test/e2e/cmd/runner run --scenario=all
```

默认把 JSON 证据写到 `test/e2e/evidence/`，该目录已被 Git 忽略。可以用
`--output-dir` 改变位置。`--keep-on-failure` 只用于交互排查；它会有意保留失败资源，
使用后需要人工删除。
