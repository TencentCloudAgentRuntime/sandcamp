# Sandcamp Runtime 镜像

这里提供 Sandcamp Runtime Image Volume 的构建定义。它与 `test/image-volume`
中的集成测试镜像分开维护。

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
