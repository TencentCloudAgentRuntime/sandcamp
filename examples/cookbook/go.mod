module github.com/TencentCloudAgentRuntime/sandcamp/examples/cookbook

go 1.24.0

require (
	github.com/TencentCloudAgentRuntime/sandcamp v0.0.0
	github.com/joho/godotenv v1.5.1
	github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags v1.3.151
	github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common v1.3.151
)

replace github.com/TencentCloudAgentRuntime/sandcamp => ../..
