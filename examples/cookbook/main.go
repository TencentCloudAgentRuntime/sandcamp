// Package main 展示 Sandcamp SDK 最小的完整接入流程：
//
//  1. 定义 Runtime/Sidecar 镜像和各进程；
//  2. 通过 RenderMounts、RenderStart 生成 AGS 配置；
//  3. 创建 custom Tool，等待其进入 ACTIVE；
//  4. 使用同一份启动配置创建 Instance。
//
// AGS Image Volume 只能从腾讯云 CCR/TCR 拉取镜像。运行前先把 Sandcamp Runtime、
// 主镜像和 Sidecar 镜像推送到自己的 CCR 或 TCR，并创建本地配置：
//
//	cp examples/cookbook/.env.example examples/cookbook/.env
//	# 编辑 examples/cookbook/.env
//	go -C examples/cookbook run .
//
// Cookbook 是独立 Go 模块，程序从自身目录读取 .env；已经存在的系统环境变量优先。
// 真实 .env 已被 Git 忽略，不要把 Secret ID、Secret Key 或 Role ARN 提交到仓库。
//
// 程序会实际创建 Tool 并启动 Instance，只输出资源 ID，不等待 Instance 进入 RUNNING，
// 也不自动清理。验证完成后需要显式停止 Instance 并删除 Tool。
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
		"SANDCAMP_SIDECAR_IMAGE",
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
			Name:              "proxy",
			Reference:         os.Getenv("SANDCAMP_SIDECAR_IMAGE"),
			ImageRegistryType: registryType,
		}},
	}
	// Image Volume 不会自动应用镜像的 OCI Entrypoint、Cmd、Env、User 或
	// WorkingDir，所以 Sidecar 的启动信息需要显式声明。Sidecar Image 和
	// Process 通过 Name 对应；Command 是完整 argv，不是 Shell 字符串。
	processes := sandcamp.Spec{
		Sidecars: []sandcamp.Process{{
			Name: "proxy",
			// sandrun 完成 pivot_root 后再启动进程，因此 Command、WorkDir，以及
			// HOME/PATH 中的文件路径都属于 proxy Sidecar 镜像的 RootFS，
			// 不是主镜像中的路径。
			Command: []string{"/usr/local/bin/python", "/opt/fastapi-proxy/app.py"},
			WorkDir: "/opt/fastapi-proxy",
			// User 省略时继承 root；这个最小示例中的所有进程都以 root 启动。
			Env: map[string]string{
				"HOME":         "/root",
				"PATH":         "/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin",
				"UPSTREAM_URL": "http://127.0.0.1:8080/",
			},
			// Expose 把 9200 写入 AGS 的对外端口配置；沙箱内部通过共享
			// Loopback 访问端口时不需要 Expose。
			Expose: []int{9200},
			// 首次成功后 campd 才继续启动后续声明；运行期间会持续探测，
			// 并把结果聚合到 campd 的 /ready。
			Probe: sandcamp.HTTPReadinessProbe("/healthz", 9200),
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
				// Probe 首次成功后才继续启动 api，并持续参与 /ready 聚合。
				Expose: []int{envdPort},
				Probe:  sandcamp.HTTPReadinessProbe("/health", envdPort),
			},
			{
				Name: "api",
				// Kind 省略时默认为 Service。
				Command: []string{"/app/server"},
				WorkDir: "/app",
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
	// Instance 使用原始 RenderStart 结果；主镜像已由 Tool 默认配置提供。
	startRequest.CustomConfiguration = configuration
	// MountOptions 保持 nil，Instance 继承 Tool.StorageMounts。

	startResponse, err := client.StartSandboxInstanceWithContext(ctx, startRequest)
	if err != nil {
		log.Fatal(err)
	}
	if startResponse.Response == nil || startResponse.Response.Instance == nil ||
		startResponse.Response.Instance.InstanceId == nil {
		log.Fatal("StartSandboxInstance did not return InstanceId")
	}

	fmt.Printf("instance_id=%s\n", *startResponse.Response.Instance.InstanceId)
}
