# Sandcamp Runtime 镜像

这里提供 Sandcamp Runtime Image Volume 的构建定义。它与 `test/image-volume`
中的集成测试镜像分开维护。

## Beta 构建产物

`main` 分支的完整 CI 通过后，会把经过 Linux 集成测试的同一份 Runtime 二进制
打包并保存到 GitHub Container Registry：

```text
ghcr.io/csjgg/sandcamp-runtime:beta
ghcr.io/csjgg/sandcamp-runtime:sha-<完整 Git Commit>
```

`beta` 会随每次成功构建移动，适合试用。需要固定运行内容时，使用 `sha-...`
标签或镜像 Digest。当前只构建 `linux/amd64`，不会发布 `latest`。

GHCR 是构建产物的分发来源，不是 AGS Image Volume 当前支持的 Registry。AGS 的
Runtime/Sidecar Image Volume 只支持腾讯云 CCR（`personal`）和 TCR
（`enterprise`）。使用前需要把镜像同步到调用方自己的 CCR 或 TCR，例如：

```bash
docker pull ghcr.io/csjgg/sandcamp-runtime:beta
docker tag \
  ghcr.io/csjgg/sandcamp-runtime:beta \
  ccr.ccs.tencentyun.com/<namespace>/sandcamp-runtime:beta
docker push ccr.ccs.tencentyun.com/<namespace>/sandcamp-runtime:beta
```

需要可复现部署时，应同步 `sha-<完整 Git Commit>` 标签或固定 Digest，而不是可变的
`beta` 标签。GHCR 包首次生成后，还需要仓库维护者在 Package Settings 中确认其读取
可见性。

## 本地构建

先在仓库根目录生成 Linux/amd64 静态二进制：

```bash
task build
```

再以仓库根目录作为 Build Context：

```bash
docker build \
  --platform linux/amd64 \
  -f images/runtime/Dockerfile \
  -t sandcamp-runtime:local \
  .
```

镜像内容：

| 路径 | 内容 | 权限 |
| --- | --- | --- |
| `/bin/campd` | 受限的 setuid `campd-launcher` | `root:root 4755` |
| `/bin/campd.real` | campd 静态二进制 | `root:root 0755` |
| `/bin/sandrun` | sandrun 静态二进制 | `root:root 0755` |

AGS 将该镜像只读挂载到 `/mnt/sandcamp`。setuid Bootstrap 要求挂载点没有
`nosuid`，并且进程的 `no_new_privs=false`。
