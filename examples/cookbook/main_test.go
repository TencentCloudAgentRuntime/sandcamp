package main

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/csjgg/sandcamp"
)

func TestBuildCookbookUsesFinalSDKContract(t *testing.T) {
	rendered, err := buildCookbook(defaultImageReferences())
	if err != nil {
		t.Fatal(err)
	}

	if len(rendered.mounts) != 3 {
		t.Fatalf("storage mounts = %d", len(rendered.mounts))
	}
	gotMounts := make([]string, 0, len(rendered.mounts))
	for _, mount := range rendered.mounts {
		if mount.ReadOnly == nil || !*mount.ReadOnly {
			t.Fatalf("mount must be read-only: %#v", mount)
		}
		gotMounts = append(gotMounts, dereference(mount.Name)+"="+dereference(mount.MountPath))
	}
	wantMounts := []string{
		"sandcamp-runtime=/mnt/sandcamp",
		"sandcamp-sidecar-egress=/mnt/sandcamp-sidecars/egress",
		"sandcamp-sidecar-fastapi=/mnt/sandcamp-sidecars/fastapi",
	}
	if !reflect.DeepEqual(gotMounts, wantMounts) {
		t.Fatalf("mounts = %v, want %v", gotMounts, wantMounts)
	}

	configuration := rendered.configuration
	if len(configuration.Command) != 1 ||
		dereference(configuration.Command[0]) != "/mnt/sandcamp/bin/campd" {
		t.Fatalf("command = %#v", configuration.Command)
	}
	if len(configuration.Args) != 1 || dereference(configuration.Args[0]) != "--" {
		t.Fatalf("args = %#v", configuration.Args)
	}
	if len(configuration.Ports) != 1 ||
		dereference(configuration.Ports[0].Name) != "port-9200" ||
		*configuration.Ports[0].Port != 9200 {
		t.Fatalf("ports = %#v", configuration.Ports)
	}
	if configuration.Probe == nil || configuration.Probe.HttpGet == nil ||
		dereference(configuration.Probe.HttpGet.Path) != "/ready" ||
		*configuration.Probe.HttpGet.Port != sandcamp.ControlPort {
		t.Fatalf("platform probe = %#v", configuration.Probe)
	}

	wire := decodeWireSpec(t, dereference(configuration.Env[0].Value))
	processes := wire["processes"].([]any)
	if len(processes) != 3 {
		t.Fatalf("process count = %d", len(processes))
	}
	egress := processes[0].(map[string]any)
	assertGeneratedSidecar(
		t,
		egress,
		"egress",
		"/mnt/sandcamp-sidecars/egress",
		"/opt/opensandbox-egress/egress",
	)
	environment := egress["env"].(map[string]any)
	var policy map[string]any
	if err := json.Unmarshal(
		[]byte(environment["OPENSANDBOX_EGRESS_RULES"].(string)),
		&policy,
	); err != nil {
		t.Fatal(err)
	}
	if policy["defaultAction"] != "deny" {
		t.Fatalf("egress policy = %#v", policy)
	}

	fastAPI := processes[1].(map[string]any)
	assertGeneratedSidecar(
		t,
		fastAPI,
		"fastapi",
		"/mnt/sandcamp-sidecars/fastapi",
		"/usr/local/bin/python",
	)
	argv := stringArguments(fastAPI["argv"].([]any))
	if !containsPair(argv, "--workdir", "/opt/fastapi-proxy") {
		t.Fatalf("fastapi argv has no generated workdir: %#v", argv)
	}
	if !containsPair(argv, "--user", "app") {
		t.Fatalf("fastapi argv has no generated user: %#v", argv)
	}

	main := processes[2].(map[string]any)
	if main["main"] != true ||
		main["workdir"] != "/opt/sandcamp-validation" ||
		!reflect.DeepEqual(
			stringArguments(main["argv"].([]any)),
			[]string{"/usr/local/bin/python", "/opt/sandcamp-validation/app.py"},
		) {
		t.Fatalf("main process = %#v", main)
	}
	user := main["user"].(map[string]any)
	if user["uid"] != float64(65532) || user["gid"] != float64(65532) {
		t.Fatalf("main user = %#v", user)
	}
}

func TestRequestsUseToolMountDefaults(t *testing.T) {
	rendered, err := buildCookbook(defaultImageReferences())
	if err != nil {
		t.Fatal(err)
	}
	toolRequest := buildToolRequest(
		rendered,
		"sandcamp-cookbook-test",
		"qcs::cam::uin/000000000:roleName/test",
		"create-test",
	)
	if len(toolRequest.StorageMounts) != 3 {
		t.Fatalf("tool mounts = %#v", toolRequest.StorageMounts)
	}
	if toolRequest.CustomConfiguration == nil ||
		dereference(toolRequest.CustomConfiguration.Image) != defaultMainImage {
		t.Fatalf("tool configuration = %#v", toolRequest.CustomConfiguration)
	}
	if len(toolRequest.CustomConfiguration.Command) != 1 ||
		toolRequest.CustomConfiguration.Probe == nil {
		t.Fatalf("tool must include default command and probe: %#v", toolRequest.CustomConfiguration)
	}

	startRequest := buildStartRequest(rendered, "tool-test", "start-test")
	if startRequest.MountOptions != nil {
		t.Fatalf("instance must inherit Tool mounts: %#v", startRequest.MountOptions)
	}
	if startRequest.CustomConfiguration != rendered.configuration {
		t.Fatal("start request did not use RenderStart output")
	}
}

func TestInspectionDoesNotMutateRenderedConfiguration(t *testing.T) {
	rendered, err := buildCookbook(defaultImageReferences())
	if err != nil {
		t.Fatal(err)
	}
	original := dereference(rendered.configuration.Env[0].Value)
	visible, encoded, err := inspectionConfiguration(rendered.configuration)
	if err != nil {
		t.Fatal(err)
	}
	if encoded != original {
		t.Fatal("inspection changed encoded payload")
	}
	if !strings.HasPrefix(dereference(visible.Env[0].Value), "<base64 payload: ") {
		t.Fatalf("visible payload = %q", dereference(visible.Env[0].Value))
	}
	if dereference(rendered.configuration.Env[0].Value) != original {
		t.Fatal("inspection mutated RenderStart output")
	}
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		arguments []string
		want      string
		wantError bool
	}{
		{want: "inspect"},
		{arguments: []string{"requests"}, want: "requests"},
		{arguments: []string{"--help"}, want: "help"},
		{arguments: []string{"unknown"}, wantError: true},
		{arguments: []string{"inspect", "requests"}, wantError: true},
	}
	for _, test := range tests {
		got, err := parseCommand(test.arguments)
		if test.wantError {
			if err == nil {
				t.Fatalf("parseCommand(%v) unexpectedly succeeded", test.arguments)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Fatalf("parseCommand(%v) = %q, %v", test.arguments, got, err)
		}
	}
}

func TestKeepSmokeResourcesIsExplicit(t *testing.T) {
	t.Setenv("SANDCAMP_COOKBOOK_KEEP_RESOURCES", "")
	if keepSmokeResources() {
		t.Fatal("empty value must not keep resources")
	}
	t.Setenv("SANDCAMP_COOKBOOK_KEEP_RESOURCES", "true")
	if !keepSmokeResources() {
		t.Fatal("true must keep resources")
	}
}

func TestSmokeRejectsPlaceholderImagesBeforeCloudCalls(t *testing.T) {
	rendered, err := buildCookbook(defaultImageReferences())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSmokeImages(rendered); err == nil {
		t.Fatal("placeholder images unexpectedly accepted")
	}
}

func assertGeneratedSidecar(
	t *testing.T,
	process map[string]any,
	name string,
	rootfs string,
	executable string,
) {
	t.Helper()
	if process["name"] != name {
		t.Fatalf("process name = %#v", process["name"])
	}
	argv := stringArguments(process["argv"].([]any))
	if len(argv) < 2 || argv[0] != "/mnt/sandcamp/bin/sandrun" {
		t.Fatalf("%s sandrun argv = %#v", name, argv)
	}
	if !containsPair(argv, "--rootfs", rootfs) ||
		!containsPair(argv, "--overlay-device", "/dev/vda") ||
		!containsPair(argv, "--overlay-id", name) {
		t.Fatalf("%s generated argv = %#v", name, argv)
	}
	if argv[len(argv)-1] != executable &&
		!contains(argv, executable) {
		t.Fatalf("%s executable is missing: %#v", name, argv)
	}
}

func containsPair(arguments []string, key, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == key && arguments[index+1] == value {
			return true
		}
	}
	return false
}

func contains(arguments []string, value string) bool {
	for _, argument := range arguments {
		if argument == value {
			return true
		}
	}
	return false
}

func stringArguments(values []any) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.(string))
	}
	return result
}

func decodeWireSpec(t *testing.T, value string) map[string]any {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(decoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
