package sandcamp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

func TestRenderMountsOwnsImageVolumePaths(t *testing.T) {
	mounts, err := RenderMounts(referenceImages())
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 3 {
		t.Fatalf("storage mounts = %d", len(mounts))
	}
	got := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		if mount.ReadOnly == nil || !*mount.ReadOnly {
			t.Fatalf("mount must be read-only: %#v", mount)
		}
		got = append(got, *mount.Name+"="+*mount.MountPath)
	}
	want := []string{
		"sandcamp-runtime=/mnt/sandcamp",
		"sandcamp-sidecar-egress=/mnt/sandcamp-sidecars/egress",
		"sandcamp-sidecar-fastapi=/mnt/sandcamp-sidecars/fastapi",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mounts = %v, want %v", got, want)
	}
}

func TestRenderStartHidesSandrunFromProcessDeclarations(t *testing.T) {
	configuration, err := RenderStart(referenceImages(), referenceSpec())
	if err != nil {
		t.Fatal(err)
	}
	if got := *configuration.Command[0]; got != defaultCampdPath {
		t.Fatalf("command = %q", got)
	}
	if len(configuration.Args) != 1 || *configuration.Args[0] != "--" {
		t.Fatalf("args must suppress the main image CMD: %#v", configuration.Args)
	}
	if got := []int64{*configuration.Ports[0].Port, *configuration.Ports[1].Port}; !reflect.DeepEqual(got, []int64{8080, 9200}) {
		t.Fatalf("ports = %v", got)
	}
	decoded := decodeRenderedSpec(t, configuration)
	if len(decoded.Processes) != 3 {
		t.Fatalf("processes = %#v", decoded.Processes)
	}
	egress := decoded.Processes[0]
	if !reflect.DeepEqual(egress.Argv, []string{
		defaultSandrunPath,
		"--rootfs", "/mnt/sandcamp-sidecars/egress",
		"--overlay-device", overlayDevicePath,
		"--overlay-id", "egress",
		"--standard-mounts",
		"--",
		"/opt/opensandbox-egress/egress",
	}) {
		t.Fatalf("egress argv = %#v", egress.Argv)
	}
	fastAPI := decoded.Processes[1]
	if !reflect.DeepEqual(fastAPI.Argv, []string{
		defaultSandrunPath,
		"--rootfs", "/mnt/sandcamp-sidecars/fastapi",
		"--overlay-device", overlayDevicePath,
		"--overlay-id", "fastapi",
		"--standard-mounts",
		"--workdir", "/opt/fastapi-proxy",
		"--",
		"/usr/local/bin/python", "/opt/fastapi-proxy/app.py",
	}) {
		t.Fatalf("fastapi argv = %#v", fastAPI.Argv)
	}
	if fastAPI.WorkDir != "" {
		t.Fatalf("campd must not apply the sidecar workdir before sandrun: %q", fastAPI.WorkDir)
	}
	main := decoded.Processes[2]
	if !main.Main || !reflect.DeepEqual(main.Argv, []string{"/app/server"}) || main.WorkDir != "/app" {
		t.Fatalf("main = %#v", main)
	}
}

func TestRenderStartEncodesNumericMainUser(t *testing.T) {
	spec := referenceSpec()
	spec.Main.User = &ProcessUser{UID: 65532, GID: 65532}
	configuration, err := RenderStart(referenceImages(), spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeRenderedSpec(t, configuration)
	got := decoded.Processes[len(decoded.Processes)-1].User
	if got == nil || got.UID != 65532 || got.GID != 65532 {
		t.Fatalf("user = %#v", got)
	}
}

func TestRenderStartEncodesNamedSidecarUserForSandrun(t *testing.T) {
	spec := referenceSpec()
	spec.Sidecars[1].User = &ProcessUser{Name: "app"}
	configuration, err := RenderStart(referenceImages(), spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeRenderedSpec(t, configuration)
	sidecar := decoded.Processes[1]
	if sidecar.User != nil {
		t.Fatalf("campd must start sandrun as root: %#v", sidecar.User)
	}
	want := []string{
		defaultSandrunPath,
		"--rootfs", "/mnt/sandcamp-sidecars/fastapi",
		"--overlay-device", overlayDevicePath,
		"--overlay-id", "fastapi",
		"--standard-mounts",
		"--workdir", "/opt/fastapi-proxy",
		"--user", "app",
		"--",
		"/usr/local/bin/python", "/opt/fastapi-proxy/app.py",
	}
	if !reflect.DeepEqual(sidecar.Argv, want) {
		t.Fatalf("sidecar argv = %#v, want %#v", sidecar.Argv, want)
	}
}

func TestRenderStartOmitsPortsWhenNothingIsExposed(t *testing.T) {
	spec := referenceSpec()
	for index := range spec.Sidecars {
		spec.Sidecars[index].Expose = nil
	}
	spec.Main.Expose = nil
	configuration, err := RenderStart(referenceImages(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Ports != nil {
		t.Fatalf("ports must be nil to preserve Tool defaults: %#v", configuration.Ports)
	}
}

func TestRenderStartRejectsMissingSidecarImage(t *testing.T) {
	images := referenceImages()
	images.Sidecars = images.Sidecars[:1]
	if _, err := RenderStart(images, referenceSpec()); !errors.Is(err, ErrMissingSidecarImage) {
		t.Fatalf("expected missing image error, got %v", err)
	}
}

func TestRenderStartValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Spec)
		want error
	}{
		{
			name: "startup budget",
			edit: func(spec *Spec) { spec.Main.StartupProbe.ReadyTimeout = 11 * time.Second },
			want: ErrStartupBudget,
		},
		{
			name: "duplicate process",
			edit: func(spec *Spec) { spec.Main.Name = "egress" },
			want: ErrDuplicateProcess,
		},
		{
			name: "duplicate port",
			edit: func(spec *Spec) { spec.Main.Expose = []int{9200} },
			want: ErrDuplicatePort,
		},
		{
			name: "reserved port",
			edit: func(spec *Spec) { spec.Main.Expose = []int{ControlPort} },
			want: ErrReservedPort,
		},
		{
			name: "relative executable",
			edit: func(spec *Spec) { spec.Sidecars[0].Command[0] = "usr/bin/egress" },
			want: ErrInvalidSpec,
		},
		{
			name: "sidecar numeric user",
			edit: func(spec *Spec) {
				spec.Sidecars[0].User = &ProcessUser{UID: 65532, GID: 65532}
			},
			want: ErrInvalidSpec,
		},
		{
			name: "invalid sidecar user name",
			edit: func(spec *Spec) {
				spec.Sidecars[0].User = &ProcessUser{Name: "../app"}
			},
			want: ErrInvalidSpec,
		},
		{
			name: "named main user",
			edit: func(spec *Spec) {
				spec.Main.User = &ProcessUser{Name: "app"}
			},
			want: ErrInvalidSpec,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := referenceSpec()
			test.edit(&spec)
			if _, err := RenderStart(referenceImages(), spec); !errors.Is(err, test.want) {
				t.Fatalf("expected %v, got %v", test.want, err)
			}
		})
	}
}

func TestRenderStartRejectsOversizedPayload(t *testing.T) {
	spec := referenceSpec()
	spec.Main.Env = map[string]string{"LARGE": strings.Repeat("x", MaxEncodedSpecBytes)}
	if _, err := RenderStart(referenceImages(), spec); !errors.Is(err, ErrSpecTooLarge) {
		t.Fatalf("expected size error, got %v", err)
	}
}

func TestRenderMountsRejectsInvalidImages(t *testing.T) {
	tests := []ImageSet{
		{},
		{
			SandcampRuntime: Image{
				Reference:         "runtime",
				ImageRegistryType: "public",
			},
		},
		{
			SandcampRuntime: referenceImages().SandcampRuntime,
			Sidecars: []SidecarImage{
				{Name: "api", Reference: "api:1", ImageRegistryType: "personal"},
				{Name: "api", Reference: "api:2", ImageRegistryType: "personal"},
			},
		},
	}
	for _, images := range tests {
		if _, err := RenderMounts(images); err == nil {
			t.Fatalf("expected invalid image error for %#v", images)
		}
	}
}

func decodeRenderedSpec(t *testing.T, configuration *ags.CustomConfiguration) wireSpec {
	t.Helper()
	if len(configuration.Env) != 1 || *configuration.Env[0].Name != SpecEnvironment {
		t.Fatalf("environment = %#v", configuration.Env)
	}
	raw, err := base64.StdEncoding.DecodeString(*configuration.Env[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded wireSpec
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func referenceImages() ImageSet {
	return ImageSet{
		SandcampRuntime: Image{
			Reference:         "ccr.example.com/team/runtime@sha256:runtime",
			ImageRegistryType: "personal",
		},
		Sidecars: []SidecarImage{
			{
				Name:              "egress",
				Reference:         "ccr.example.com/team/egress@sha256:egress",
				ImageRegistryType: "personal",
			},
			{
				Name:              "fastapi",
				Reference:         "ccr.example.com/team/fastapi@sha256:fastapi",
				ImageRegistryType: "personal",
			},
		},
	}
}

func referenceSpec() Spec {
	return Spec{
		Sidecars: []Process{
			{
				Name:    "egress",
				Command: []string{"/opt/opensandbox-egress/egress"},
				Env: map[string]string{
					"OPENSANDBOX_EGRESS_MODE":  "dns",
					"OPENSANDBOX_EGRESS_RULES": `{"defaultAction":"deny"}`,
				},
				StartupProbe: HTTPStartupProbe("/healthz", 24774),
			},
			{
				Name:         "fastapi",
				Command:      []string{"/usr/local/bin/python", "/opt/fastapi-proxy/app.py"},
				WorkDir:      "/opt/fastapi-proxy",
				Expose:       []int{9200},
				StartupProbe: HTTPStartupProbe("/healthz", 9200),
			},
		},
		Main: Process{
			Name:         "app",
			Command:      []string{"/app/server"},
			WorkDir:      "/app",
			Expose:       []int{8080},
			StartupProbe: HTTPStartupProbe("/healthz", 8080),
		},
	}
}
