// Package main 展示 Sandcamp SDK 最小的完整接入流程：
//
//  1. 定义 Runtime/Sidecar 镜像和各进程；
//  2. 通过 RenderMounts、RenderStart 生成 AGS 配置；
//  3. 创建 custom Tool，等待其进入 ACTIVE；
//  4. 使用同一份启动配置创建 Instance。
//
// 示例使用固定 Linux/amd64 Manifest 的公开上游镜像：
//
//   - Main: docker.io/library/bash@sha256:534a5f1d11652aadaa9f08838f6637ac11a46a8b4b736a4cbf09c5945e38516f
//   - Sidecar: docker.io/opensandbox/egress@sha256:0dd9727216b535fa34ef77495c3e465da848b888c28ee4be4f92c1003a97a71f
//
// Docker Official Bash 镜像约 6.5 MB，自带 Bash 以及 BusyBox 的 ps、wget、
// nslookup 和 nc，登录沙箱后不需要再安装排查工具。
// AGS 镜像引用需要使用平台支持的个人版或企业版镜像仓库；运行前先把
// 上述两个镜像和 Sandcamp Runtime 同步到自己的腾讯云镜像仓库，并创建本地配置：
//
//	cp examples/cookbook/.env.example examples/cookbook/.env
//	# 编辑 examples/cookbook/.env
//	go -C examples/cookbook run .
//
// Cookbook 是独立 Go 模块，程序从自身目录读取 .env；已经存在的系统环境变量优先。
// 真实 .env 已被 Git 忽略，不要把 Secret ID、Secret Key 或 Role ARN 提交到仓库。
//
// 程序会实际创建 Tool、启动 Instance，等待其进入 RUNNING，然后打印登录与
// 验证命令，其中包括 Main 与 Egress Sidecar 双向读写同一目录。程序不自动清理；
// 验证完成后需要显式停止 Instance 并删除 Tool。
// Runtime Image Volume 提供 campd 和 sandrun。campd 负责进程启动、信号转发和聚合
// 探针；sandrun 使用独立 Mount Namespace 和 OverlayFS RootFS 启动 Sidecar。它不创建
// PID 或 Network Namespace，因此 Main 与 Sidecar 进程相互可见并共享 Loopback；这不是
// 额外的安全隔离边界。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/TencentCloudAgentRuntime/sandcamp"
	"github.com/joho/godotenv"
	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
)

const (
	// 这是 AGS 对外提供的 envd 镜像。Cookbook 只挂载其中的单个静态可执行文件，
	// 不把该镜像作为 Sidecar RootFS 使用。
	envdImage     = "ccr.ccs.tencentyun.com/ags-image/envd:fixed-0.6.13"
	envdSubPath   = "/usr/bin/envd"
	envdMountPath = "/mnt/envd-runtime/envd"
	envdPort      = 49983
	sharedSource  = "/sandcamp-share/source"
	sharedTarget  = "/sandcamp-share/egress"
)

func main() {
	if err := godotenv.Load(".env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("load .env: %v", err)
	}

	required := []string{
		"TENCENTCLOUD_SECRET_ID",
		"TENCENTCLOUD_SECRET_KEY",
		"TENCENTCLOUD_REGION",
		"AGS_ROLE_ARN",
		"SANDCAMP_MAIN_IMAGE",
		"SANDCAMP_RUNTIME_IMAGE",
		"SANDCAMP_EGRESS_IMAGE",
		"SANDCAMP_IMAGE_REGISTRY_TYPE",
	}
	for _, name := range required {
		if os.Getenv(name) == "" {
			log.Fatalf("missing environment variable %s", name)
		}
	}

	// 为了让示例保持紧凑，主镜像、Runtime 和 Sidecar 使用同一种 Registry 类型。
	// personal 对应 CCR；enterprise 对应 TCR。SDK 的每个 Image 字段也可以分别指定。
	registryType := sandcamp.ImageRegistryType(os.Getenv("SANDCAMP_IMAGE_REGISTRY_TYPE"))
	// 主镜像由 Tool.CustomConfiguration.Image 指定，不放入 ImageSet。
	// ImageSet 只描述通过 Image Volume 挂载的 Sandcamp Runtime 和 Sidecar。
	images := sandcamp.ImageSet{
		SandcampRuntime: sandcamp.Image{
			Reference:         os.Getenv("SANDCAMP_RUNTIME_IMAGE"),
			ImageRegistryType: registryType,
		},
		Sidecars: []sandcamp.SidecarImage{{
			Name:              "egress",
			Reference:         os.Getenv("SANDCAMP_EGRESS_IMAGE"),
			ImageRegistryType: registryType,
		}},
	}
	// Image Volume 不会自动应用镜像的 OCI Entrypoint、Cmd、Env、User 或
	// WorkingDir，所以 Sidecar 的启动信息需要显式声明。Sidecar Image 和
	// Process 通过 Name 对应；Command 是完整 argv，不是 Shell 字符串。
	processes := sandcamp.Spec{
		Sidecars: []sandcamp.Process{{
			Name: "egress",
			// Image Volume 不会自动应用 OCI Entrypoint，因此这里显式复现
			// OpenSandbox Egress 官方镜像的 argv。下面所有路径都来自
			// Egress Sidecar RootFS，而不是 Bash 主镜像。
			Command: []string{
				"/opt/opensandbox-egress/supervisor",
				"--pre-start=/opt/opensandbox-egress/cleanup.sh",
				"--name=egress",
				"--grace-period=20s",
				"--",
				"/opt/opensandbox-egress/egress",
			},
			// User 省略时以 root 启动。Egress 会在共享 Network Namespace
			// 中设置 DNS 转发规则，因此它会影响 Main 和其他 Sidecar。
			Env: map[string]string{
				"OPENSANDBOX_EGRESS_MODE":      "dns",
				"OPENSANDBOX_EGRESS_HTTP_ADDR": ":24774",
				"OPENSANDBOX_EGRESS_RULES":     `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"},{"action":"allow","target":"*.example.com"}]}`,
				"PATH":                         "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			},
			// Source 从主 Mount Namespace 解析，Target 从 Egress RootFS
			// 解析。两个路径都故意不预置，由 sandrun 自动创建为 0777
			// 目录。Sidecar 启动后，双方看到的是同一目录和同一批 inode；
			// ReadOnly 省略时为 false，Main 与 Egress 都可以写入。
			Mounts: []sandcamp.BindMount{{
				Source: sharedSource,
				Target: sharedTarget,
			}},
			// Expose 把 24774 写入 AGS 的对外端口配置；沙箱内部通过共享
			// Loopback 访问端口时不需要 Expose。
			Expose: []int{24774},
			// 首次成功后 campd 才继续启动后续声明；运行期间会持续探测，
			// 并把结果聚合到 campd 的 /ready。
			Probe: sandcamp.HTTPReadinessProbe("/healthz", 24774),
		}},
		// Main 是数组；每一项都在 AGS 主 Mount Namespace 中执行。Sidecars
		// 完成启动 Gate 后，Main 再按声明顺序处理。Main 的可执行文件既可以
		// 来自主镜像，也可以来自 Tool.StorageMounts。
		Main: []sandcamp.Process{
			{
				Name:    "envd",
				Kind:    sandcamp.Service,
				Command: []string{envdMountPath, "-port", fmt.Sprint(envdPort)},
				// envd 作为 Main Service 由 campd 管理，不经过 sandrun。
				// Probe 首次成功后才继续启动 main-http，并持续参与 /ready 聚合。
				Expose: []int{envdPort},
				Probe:  sandcamp.HTTPReadinessProbe("/health", envdPort),
			},
			{
				Name: "main-http",
				// Kind 省略时默认为 Service。这些路径来自 Bash
				// 主镜像；User 省略时以 root 启动。官方镜像把 Bash
				// 安装在 /usr/local/bin，而 envd 远程终端调用 /bin/bash，
				// 所以启动时显式创建软链接。
				Command: []string{
					"/usr/local/bin/bash",
					"-c",
					fmt.Sprintf(`ln -sf /usr/local/bin/bash /bin/bash
printf 'created by Main\n' > %s/sandcamp-from-main.txt
cat > /tmp/sandcamp-http-response <<'EOF'
#!/usr/local/bin/bash
printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 17\r\nConnection: close\r\n\r\nsandcamp main ok\n'
EOF
chmod 0755 /tmp/sandcamp-http-response
exec /usr/bin/nc -lk -p 8080 -e /tmp/sandcamp-http-response`, sharedSource),
				},
				WorkDir: "/",
				Expose:  []int{8080},
				Probe:   sandcamp.HTTPReadinessProbe("/healthz", 8080),
			},
		},
	}

	// RenderMounts 生成 Runtime/Sidecar 的只读 StorageMounts。
	mounts, err := sandcamp.RenderMounts(images)
	if err != nil {
		log.Fatal(err)
	}
	// envd 不是 Sandcamp Sidecar：文件如何进入主 Mount Namespace 由调用方
	// 显式配置，进程本身则作为上面的 Main Service 交给 campd 管理。
	mounts = append(mounts, &ags.StorageMount{
		Name:      common.StringPtr("envd-runtime"),
		MountPath: common.StringPtr(envdMountPath),
		ReadOnly:  common.BoolPtr(true),
		StorageSource: &ags.StorageSource{Image: &ags.ImageStorageSource{
			Reference:         common.StringPtr(envdImage),
			ImageRegistryType: common.StringPtr(string(sandcamp.ImageRegistryPersonal)),
			SubPath:           common.StringPtr(envdSubPath),
		}},
	})
	// RenderStart 校验进程声明，并生成 campd Command、SANDCAMP_SPEC、端口和
	// AGS 唯一的 /ready 探针。SDK 保留结构化进程声明；Sidecar 的 sandrun
	// 参数由 campd 根据 RootFS、WorkDir、User 和 Command 生成。
	configuration, err := sandcamp.RenderStart(images, processes)
	if err != nil {
		log.Fatal(err)
	}

	// 先打印 RenderMounts/RenderStart 的完整输出，便于在发送请求前检查挂载和启动配置。
	mountsJSON, err := json.MarshalIndent(mounts, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	configurationJSON, err := json.MarshalIndent(configuration, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	// SANDCAMP_SPEC 在 AGS Env 中使用 Base64 传输；这里额外解码一次，方便
	// 直接查看交给 campd 的 runtime declaration。
	runtimePayload, err := base64.StdEncoding.DecodeString(*configuration.Env[0].Value)
	if err != nil {
		log.Fatal(err)
	}
	var runtimeDeclaration any
	if err := json.Unmarshal(runtimePayload, &runtimeDeclaration); err != nil {
		log.Fatal(err)
	}
	runtimeJSON, err := json.MarshalIndent(runtimeDeclaration, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf(
		"mounts:\n%s\nconfiguration:\n%s\nruntime declaration:\n%s\n",
		mountsJSON,
		configurationJSON,
		runtimeJSON,
	)

	credential := common.NewCredential(
		os.Getenv("TENCENTCLOUD_SECRET_ID"),
		os.Getenv("TENCENTCLOUD_SECRET_KEY"),
	)
	client, err := ags.NewClient(
		credential,
		os.Getenv("TENCENTCLOUD_REGION"),
		profile.NewClientProfile(),
	)
	if err != nil {
		log.Fatal(err)
	}

	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	// AGS custom Tool 要求默认 Command 和 Probe，因此 Tool 也使用 RenderStart
	// 的结果。复制一份后，再补充只属于 Tool 的主镜像和资源配置。
	toolConfiguration := *configuration
	toolConfiguration.Image = common.StringPtr(os.Getenv("SANDCAMP_MAIN_IMAGE"))
	toolConfiguration.ImageRegistryType = common.StringPtr(string(registryType))
	toolConfiguration.Resources = &ags.ResourceConfiguration{
		CPU:    common.StringPtr("2"),
		Memory: common.StringPtr("4Gi"),
	}

	createRequest := ags.NewCreateSandboxToolRequest()
	createRequest.ToolName = common.StringPtr("sandcamp-cookbook-" + runID)
	createRequest.ToolType = common.StringPtr("custom")
	createRequest.Description = common.StringPtr("Sandcamp SDK cookbook")
	createRequest.DefaultTimeout = common.StringPtr("15m")
	createRequest.ClientToken = common.StringPtr("sandcamp-create-" + runID)
	createRequest.RoleArn = common.StringPtr(os.Getenv("AGS_ROLE_ARN"))
	createRequest.NetworkConfiguration = &ags.NetworkConfiguration{
		NetworkMode: common.StringPtr("PUBLIC"),
	}
	createRequest.StorageMounts = mounts
	createRequest.CustomConfiguration = &toolConfiguration

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	createResponse, err := client.CreateSandboxToolWithContext(ctx, createRequest)
	if err != nil {
		log.Fatal(err)
	}
	if createResponse.Response == nil || createResponse.Response.ToolId == nil {
		log.Fatal("CreateSandboxTool did not return ToolId")
	}
	toolID := *createResponse.Response.ToolId
	fmt.Printf("tool_id=%s\n", toolID)

	// Tool 创建是异步操作；进入 ACTIVE 后才能启动 Instance。
	for {
		describeRequest := ags.NewDescribeSandboxToolListRequest()
		describeRequest.ToolIds = common.StringPtrs([]string{toolID})
		describeResponse, err := client.DescribeSandboxToolListWithContext(ctx, describeRequest)
		if err != nil {
			log.Fatal(err)
		}
		if describeResponse.Response != nil &&
			len(describeResponse.Response.SandboxToolSet) == 1 {
			tool := describeResponse.Response.SandboxToolSet[0]
			if tool.Status != nil && *tool.Status == "ACTIVE" {
				break
			}
			if tool.Status != nil && *tool.Status == "FAILED" {
				reason := ""
				if tool.StatusReason != nil {
					reason = *tool.StatusReason
				}
				log.Fatalf("tool failed: %s", reason)
			}
		}
		select {
		case <-ctx.Done():
			log.Fatal(ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}

	startRequest := ags.NewStartSandboxInstanceRequest()
	startRequest.ToolId = common.StringPtr(toolID)
	startRequest.Timeout = common.StringPtr("15m")
	startRequest.ClientToken = common.StringPtr("sandcamp-start-" + runID)
	startRequest.AuthMode = common.StringPtr("TOKEN")
	// CustomConfiguration 保持 nil，Instance 完整继承 Tool 中已经保存的
	// 主镜像、campd 启动配置、SANDCAMP_SPEC、Ports、Probe 和资源配置。
	// MountOptions 也保持 nil，Instance 继承 Tool.StorageMounts。

	startResponse, err := client.StartSandboxInstanceWithContext(ctx, startRequest)
	if err != nil {
		log.Fatal(err)
	}
	if startResponse.Response == nil || startResponse.Response.Instance == nil ||
		startResponse.Response.Instance.InstanceId == nil {
		log.Fatal("StartSandboxInstance did not return InstanceId")
	}

	instanceID := *startResponse.Response.Instance.InstanceId
	fmt.Printf("instance_id=%s\n", instanceID)

	// 等待聚合探针通过，让后面打印的登录与查看命令可以直接执行。
	for {
		describeRequest := ags.NewDescribeSandboxInstanceListRequest()
		describeRequest.InstanceIds = common.StringPtrs([]string{instanceID})
		describeResponse, err := client.DescribeSandboxInstanceListWithContext(ctx, describeRequest)
		if err != nil {
			log.Fatal(err)
		}
		if describeResponse.Response != nil && len(describeResponse.Response.InstanceSet) == 1 {
			instance := describeResponse.Response.InstanceSet[0]
			if instance.Status != nil && *instance.Status == "RUNNING" {
				break
			}
			if instance.Status != nil && (*instance.Status == "FAILED" || *instance.Status == "STOPPED") {
				log.Fatalf("instance entered terminal status %s", *instance.Status)
			}
		}
		select {
		case <-ctx.Done():
			log.Fatal(ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}

	fmt.Printf(`instance_status=RUNNING

先让 agr 继承 Cookbook 的本地凭据（不会打印凭据）：
  set -a; source .env; set +a

登录主镜像：
  agr instance login %s --user root

登录后可直接执行：
  ps -ef
  wget -qO- http://127.0.0.1:24774/healthz
  wget -qO- http://127.0.0.1:8080/healthz
  wget -S -O /dev/null http://127.0.0.1:49983/health
  nslookup example.com
  nslookup example.org  # 预期被 Egress 策略拒绝

验证自动创建的 Source 与 Egress Target：
  egress_pid="$(ps -o pid,args | awk '$2 == "/opt/opensandbox-egress/supervisor" {print $1; exit}')"
  echo "egress supervisor pid=$egress_pid"

  stat -c 'Source: %%a %%u:%%g %%n' /sandcamp-share/source
  nsenter -t "$egress_pid" -m -r -w -- \
    stat -c 'Target: %%a %%u:%%g %%n' /sandcamp-share/egress

  # Main 启动时写入，进入 Egress 的 Mount Namespace 后可以直接读取。
  cat /sandcamp-share/source/sandcamp-from-main.txt
  nsenter -t "$egress_pid" -m -r -w -- \
    cat /sandcamp-share/egress/sandcamp-from-main.txt

  # 从 Egress RootFS 写入，同一文件立即出现在 Main 中。
  nsenter -t "$egress_pid" -m -r -w -- /bin/sh -c \
    'printf "created by Egress Sidecar\n" > /sandcamp-share/egress/sandcamp-from-egress.txt'
  cat /sandcamp-share/source/sandcamp-from-egress.txt

  # 两边的 device:inode 相同，说明它是 Bind 共享，不是文件复制。
  stat -c 'Main: %%d:%%i %%n' /sandcamp-share/source/sandcamp-from-egress.txt
  nsenter -t "$egress_pid" -m -r -w -- \
    stat -c 'Sidecar: %%d:%%i %%n' /sandcamp-share/egress/sandcamp-from-egress.txt
`, instanceID)
}
