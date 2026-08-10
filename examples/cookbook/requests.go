package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/csjgg/sandcamp"
	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

func writeInspection(rendered *cookbook) error {
	configuration, encoded, err := inspectionConfiguration(rendered.configuration)
	if err != nil {
		return err
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("解码 SANDCAMP_SPEC: %w", err)
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return fmt.Errorf("解析 SANDCAMP_SPEC: %w", err)
	}
	return writeJSON(struct {
		Images           sandcamp.ImageSet        `json:"images"`
		Processes        sandcamp.Spec            `json:"processes"`
		StorageMounts    []*ags.StorageMount      `json:"storage_mounts"`
		AGSConfiguration *ags.CustomConfiguration `json:"ags_custom_configuration"`
		DecodedSpec      any                      `json:"decoded_sandcamp_spec"`
	}{
		Images:           rendered.images,
		Processes:        rendered.processes,
		StorageMounts:    rendered.mounts,
		AGSConfiguration: configuration,
		DecodedSpec:      decoded,
	})
}

func inspectionConfiguration(
	configuration *ags.CustomConfiguration,
) (*ags.CustomConfiguration, string, error) {
	cloned := *configuration
	cloned.Env = make([]*ags.EnvVar, 0, len(configuration.Env))
	var encoded string
	for _, source := range configuration.Env {
		if source == nil {
			continue
		}
		variable := *source
		if dereference(variable.Name) == sandcamp.SpecEnvironment {
			encoded = dereference(variable.Value)
			variable.Value = stringPointer(fmt.Sprintf("<base64 payload: %d bytes>", len(encoded)))
		}
		cloned.Env = append(cloned.Env, &variable)
	}
	if encoded == "" {
		return nil, "", errors.New("RenderStart 输出缺少 SANDCAMP_SPEC")
	}
	return &cloned, encoded, nil
}

func writeRequests(rendered *cookbook) error {
	const (
		exampleToolID  = "replace-with-created-tool-id"
		exampleRoleARN = "qcs::cam::uin/000000000:roleName/replace-with-image-pull-role"
	)
	return writeJSON(struct {
		CreateTool    *ags.CreateSandboxToolRequest    `json:"create_tool_request"`
		StartInstance *ags.StartSandboxInstanceRequest `json:"start_instance_request"`
	}{
		CreateTool: buildToolRequest(
			rendered,
			"sandcamp-cookbook-example",
			exampleRoleARN,
			"sandcamp-cookbook-create-example",
		),
		StartInstance: buildStartRequest(
			rendered,
			exampleToolID,
			"sandcamp-cookbook-start-example",
		),
	})
}

func buildToolRequest(
	rendered *cookbook,
	toolName string,
	roleARN string,
	clientToken string,
) *ags.CreateSandboxToolRequest {
	request := ags.NewCreateSandboxToolRequest()
	request.ToolName = stringPointer(toolName)
	request.ToolType = stringPointer("custom")
	request.Description = stringPointer("Temporary Tool created by the Sandcamp Cookbook")
	request.NetworkConfiguration = &ags.NetworkConfiguration{
		NetworkMode: stringPointer("PUBLIC"),
	}
	request.DefaultTimeout = stringPointer("15m")
	request.ClientToken = stringPointer(clientToken)
	request.RoleArn = stringPointer(roleARN)
	request.StorageMounts = append(request.StorageMounts, rendered.mounts...)

	// custom Tool 要求默认 Command 和 Probe，因此 Tool 与 Instance 都复用
	// RenderStart 的结果；这里只补充 Tool 侧的主镜像和资源配置。
	configuration := *rendered.configuration
	configuration.Image = stringPointer(rendered.mainImage)
	configuration.ImageRegistryType = stringPointer(rendered.registryType)
	configuration.Resources = &ags.ResourceConfiguration{
		CPU:    stringPointer("2"),
		Memory: stringPointer("4Gi"),
	}
	request.CustomConfiguration = &configuration
	return request
}

func buildStartRequest(
	rendered *cookbook,
	toolID string,
	clientToken string,
) *ags.StartSandboxInstanceRequest {
	request := ags.NewStartSandboxInstanceRequest()
	request.ToolId = stringPointer(toolID)
	request.Timeout = stringPointer("15m")
	request.ClientToken = stringPointer(clientToken)
	request.AuthMode = stringPointer("TOKEN")
	// MountOptions 保持 nil，继承 Tool.StorageMounts。
	request.CustomConfiguration = rendered.configuration
	return request
}

func writeJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func stringPointer(value string) *string { return &value }
