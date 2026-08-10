package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
)

type smokeResult struct {
	ToolID         string `json:"tool_id"`
	InstanceID     string `json:"instance_id"`
	InstanceStatus string `json:"instance_status"`
	Cleanup        string `json:"cleanup"`
}

func runSmoke(parent context.Context, rendered *cookbook) (returnErr error) {
	if err := validateSmokeImages(rendered); err != nil {
		return err
	}
	roleARN, err := requiredEnvironment("AGS_ROLE_ARN")
	if err != nil {
		return err
	}
	client, err := newAGSClient()
	if err != nil {
		return err
	}
	runID := fmt.Sprintf("%x", time.Now().UnixNano())
	keepResources := keepSmokeResources()
	ctx, cancel := context.WithTimeout(parent, 12*time.Minute)
	defer cancel()

	var toolID string
	var instanceID string
	cleaned := false
	defer func() {
		if returnErr == nil || cleaned {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		if cleanupErr := cleanup(cleanupCtx, client, instanceID, toolID); cleanupErr != nil {
			fmt.Fprintf(
				os.Stderr,
				"自动清理失败（instance=%s tool=%s）：%v\n",
				instanceID,
				toolID,
				cleanupErr,
			)
		}
	}()

	fmt.Fprintln(os.Stderr, "1/5 创建临时 AGS Tool")
	createRequest := buildToolRequest(
		rendered,
		"sandcamp-cookbook-"+runID,
		roleARN,
		"sandcamp-create-"+runID,
	)
	createResponse, err := client.CreateSandboxToolWithContext(ctx, createRequest)
	if err != nil {
		return fmt.Errorf("CreateSandboxTool: %w", err)
	}
	if createResponse.Response == nil || createResponse.Response.ToolId == nil {
		return errors.New("CreateSandboxTool 未返回 ToolId")
	}
	toolID = *createResponse.Response.ToolId

	fmt.Fprintf(os.Stderr, "2/5 等待 Tool %s 进入 ACTIVE\n", toolID)
	if _, err := waitTool(ctx, client, toolID, "ACTIVE"); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "3/5 启动 Sandbox Instance（不发送 MountOptions）")
	startResponse, err := client.StartSandboxInstanceWithContext(
		ctx,
		buildStartRequest(rendered, toolID, "sandcamp-start-"+runID),
	)
	if err != nil {
		return fmt.Errorf("StartSandboxInstance: %w", err)
	}
	if startResponse.Response == nil || startResponse.Response.Instance == nil ||
		startResponse.Response.Instance.InstanceId == nil {
		return errors.New("StartSandboxInstance 未返回 InstanceId")
	}
	instanceID = *startResponse.Response.Instance.InstanceId

	fmt.Fprintf(os.Stderr, "4/5 等待 Instance %s 进入 RUNNING\n", instanceID)
	instance, err := waitInstance(ctx, client, instanceID, "RUNNING")
	if err != nil {
		return err
	}
	if keepResources {
		fmt.Fprintln(os.Stderr, "5/5 保留 Instance 和 Tool，供登录检查")
		return writeJSON(smokeResult{
			ToolID:         toolID,
			InstanceID:     instanceID,
			InstanceStatus: dereference(instance.Status),
			Cleanup:        "resources kept; stop the instance and delete the tool manually",
		})
	}

	fmt.Fprintln(os.Stderr, "5/5 停止 Instance 并删除临时 Tool")
	if err := cleanup(ctx, client, instanceID, toolID); err != nil {
		return err
	}
	cleaned = true
	return writeJSON(smokeResult{
		ToolID:         toolID,
		InstanceID:     instanceID,
		InstanceStatus: dereference(instance.Status),
		Cleanup:        "instance stopped; tool deleted",
	})
}

func validateSmokeImages(rendered *cookbook) error {
	references := []struct {
		name      string
		reference string
	}{
		{name: "main", reference: rendered.mainImage},
		{name: "runtime", reference: rendered.images.SandcampRuntime.Reference},
	}
	for _, sidecar := range rendered.images.Sidecars {
		references = append(references, struct {
			name      string
			reference string
		}{name: sidecar.Name, reference: sidecar.Reference})
	}
	for _, image := range references {
		if strings.HasPrefix(image.reference, "registry.example.com/") {
			return fmt.Errorf(
				"镜像 %s 仍是占位引用；请设置 SANDCAMP_COOKBOOK_*_IMAGE 环境变量",
				image.name,
			)
		}
	}
	return nil
}

func keepSmokeResources() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SANDCAMP_COOKBOOK_KEEP_RESOURCES"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func cleanup(
	ctx context.Context,
	client *ags.Client,
	instanceID string,
	toolID string,
) error {
	var result error
	if instanceID != "" {
		request := ags.NewStopSandboxInstanceRequest()
		request.InstanceId = stringPointer(instanceID)
		if _, err := client.StopSandboxInstanceWithContext(ctx, request); err != nil {
			result = errors.Join(result, fmt.Errorf("StopSandboxInstance: %w", err))
		} else if _, err := waitInstance(ctx, client, instanceID, "STOPPED"); err != nil {
			result = errors.Join(result, err)
		}
	}
	if toolID != "" {
		request := ags.NewDeleteSandboxToolRequest()
		request.ToolId = stringPointer(toolID)
		if _, err := client.DeleteSandboxToolWithContext(ctx, request); err != nil {
			result = errors.Join(result, fmt.Errorf("DeleteSandboxTool: %w", err))
		} else if err := waitToolDeleted(ctx, client, toolID); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func waitTool(
	ctx context.Context,
	client *ags.Client,
	toolID string,
	expected string,
) (*ags.SandboxTool, error) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		request := ags.NewDescribeSandboxToolListRequest()
		request.ToolIds = []*string{stringPointer(toolID)}
		response, err := client.DescribeSandboxToolListWithContext(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("DescribeSandboxToolList: %w", err)
		}
		if response.Response != nil && len(response.Response.SandboxToolSet) == 1 {
			tool := response.Response.SandboxToolSet[0]
			status := dereference(tool.Status)
			if status == expected {
				return tool, nil
			}
			if status == "FAILED" {
				return nil, fmt.Errorf(
					"Tool %s 进入 FAILED：%s",
					toolID,
					dereference(tool.StatusReason),
				)
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("等待 Tool %s 达到 %s：%w", toolID, expected, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitToolDeleted(ctx context.Context, client *ags.Client, toolID string) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		request := ags.NewDescribeSandboxToolListRequest()
		request.ToolIds = []*string{stringPointer(toolID)}
		response, err := client.DescribeSandboxToolListWithContext(ctx, request)
		if err != nil {
			return fmt.Errorf("DescribeSandboxToolList: %w", err)
		}
		if response.Response != nil && len(response.Response.SandboxToolSet) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 Tool %s 删除：%w", toolID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitInstance(
	ctx context.Context,
	client *ags.Client,
	instanceID string,
	expected string,
) (*ags.SandboxInstance, error) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		request := ags.NewDescribeSandboxInstanceListRequest()
		request.InstanceIds = []*string{stringPointer(instanceID)}
		response, err := client.DescribeSandboxInstanceListWithContext(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("DescribeSandboxInstanceList: %w", err)
		}
		if response.Response != nil && len(response.Response.InstanceSet) == 1 {
			instance := response.Response.InstanceSet[0]
			status := dereference(instance.Status)
			if status == expected {
				return instance, nil
			}
			if status == "FAILED" || status == "STOP_FAILED" ||
				(expected == "RUNNING" && status == "STOPPED") {
				return nil, fmt.Errorf(
					"Instance %s 进入终态 %s：%s",
					instanceID,
					status,
					dereference(instance.StopReason),
				)
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("等待 Instance %s 达到 %s：%w", instanceID, expected, ctx.Err())
		case <-ticker.C:
		}
	}
}

func newAGSClient() (*ags.Client, error) {
	secretID, err := requiredEnvironment("TENCENTCLOUD_SECRET_ID")
	if err != nil {
		return nil, err
	}
	secretKey, err := requiredEnvironment("TENCENTCLOUD_SECRET_KEY")
	if err != nil {
		return nil, err
	}
	region, err := requiredEnvironment("TENCENTCLOUD_REGION")
	if err != nil {
		return nil, err
	}
	var credential common.CredentialIface = common.NewCredential(secretID, secretKey)
	if token := strings.TrimSpace(os.Getenv("TENCENTCLOUD_TOKEN")); token != "" {
		credential = common.NewTokenCredential(secretID, secretKey, token)
	}
	return ags.NewClient(credential, region, profile.NewClientProfile())
}

func requiredEnvironment(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("缺少环境变量 %s", name)
	}
	return value, nil
}
