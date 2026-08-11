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

func TestImageRegistryTypesMatchAGSValues(t *testing.T) {
	if got := string(ImageRegistryPersonal); got != "personal" {
		t.Fatalf("personal registry type = %q", got)
	}
	if got := string(ImageRegistryEnterprise); got != "enterprise" {
		t.Fatalf("enterprise registry type = %q", got)
	}
}

func TestRenderStartEncodesDeclarativeRuntimeSpec(t *testing.T) {
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

	decoded, raw := decodeRuntimeSpec(t, configuration)
	if decoded.Version != 2 {
		t.Fatalf("version = %d", decoded.Version)
	}
	if strings.Contains(string(raw), "sandrun") {
		t.Fatalf("SDK must preserve declarations instead of rendering sandrun argv: %s", raw)
	}
	if len(decoded.Sidecars) != 2 || len(decoded.Main) != 3 {
		t.Fatalf("runtime declaration = %#v", decoded)
	}

	egress := decoded.Sidecars[0]
	if egress.Name != "egress" || egress.Kind != Service {
		t.Fatalf("egress identity = %#v", egress)
	}
	if egress.RootFS != "/mnt/sandcamp-sidecars/egress" {
		t.Fatalf("egress rootfs = %q", egress.RootFS)
	}
	if egress.OverlayDevice != defaultOverlayDevicePath || !egress.StandardMounts {
		t.Fatalf("egress filesystem options = %#v", egress)
	}
	if !reflect.DeepEqual(egress.Command, []string{"/opt/opensandbox-egress/egress"}) {
		t.Fatalf("egress command = %#v", egress.Command)
	}
	if egress.ReadinessProbe == nil || egress.ReadinessProbe.StartupTimeoutMS != 3_000 {
		t.Fatalf("egress readiness probe = %#v", egress.ReadinessProbe)
	}

	fastAPI := decoded.Sidecars[1]
	if fastAPI.RootFS != "/mnt/sandcamp-sidecars/fastapi" || fastAPI.WorkDir != "/opt/fastapi-proxy" {
		t.Fatalf("fastapi = %#v", fastAPI)
	}
	if fastAPI.User == nil || fastAPI.User.Name != "app" {
		t.Fatalf("fastapi user = %#v", fastAPI.User)
	}
	if fastAPI.ReadinessProbe == nil ||
		fastAPI.ReadinessProbe.FailureThreshold != 4 ||
		fastAPI.ReadinessProbe.SuccessThreshold != 2 {
		t.Fatalf("fastapi readiness probe = %#v", fastAPI.ReadinessProbe)
	}

	prepare := decoded.Main[0]
	if prepare.Kind != RunToCompletion || prepare.CompletionTimeoutMS != 2_000 {
		t.Fatalf("prepare = %#v", prepare)
	}
	if prepare.ReadinessProbe != nil {
		t.Fatalf("run-to-completion process has readiness probe: %#v", prepare.ReadinessProbe)
	}

	app := decoded.Main[1]
	if app.Kind != Service || app.WorkDir != "/app" {
		t.Fatalf("app = %#v", app)
	}
	if app.User == nil || app.User.UID != 65532 || app.User.GID != 65532 {
		t.Fatalf("app user = %#v", app.User)
	}

	worker := decoded.Main[2]
	if worker.Kind != Service || worker.ReadinessProbe != nil {
		t.Fatalf("worker = %#v", worker)
	}
}

func TestRenderStartSupportsSidecarOnlySpec(t *testing.T) {
	spec := Spec{
		Sidecars: []Process{{
			Name:    "egress",
			Command: []string{"/opt/opensandbox-egress/egress"},
		}},
	}
	configuration, err := RenderStart(referenceImages(), spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded, raw := decodeRuntimeSpec(t, configuration)
	if len(decoded.Sidecars) != 1 || len(decoded.Main) != 0 {
		t.Fatalf("runtime declaration = %#v", decoded)
	}
	if !strings.Contains(string(raw), `"main":[]`) {
		t.Fatalf("empty main group must be encoded as an array: %s", raw)
	}
}

func TestRenderStartSupportsMultipleMainProcesses(t *testing.T) {
	spec := referenceSpec()
	configuration, err := RenderStart(referenceImages(), spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := decodeRuntimeSpec(t, configuration)
	got := make([]string, 0, len(decoded.Main))
	for _, process := range decoded.Main {
		got = append(got, process.Name)
	}
	if want := []string{"prepare", "app", "worker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("main process order = %v, want %v", got, want)
	}
}

func TestRenderStartEncodesExplicitRootMainUser(t *testing.T) {
	spec := referenceSpec()
	spec.Main[2].User = &ProcessUser{UID: 0, GID: 0}
	configuration, err := RenderStart(referenceImages(), spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := decodeRuntimeSpec(t, configuration)
	got := decoded.Main[2].User
	if got == nil || got.UID != 0 || got.GID != 0 {
		t.Fatalf("root user = %#v", got)
	}
}

func TestRenderStartOmitsPortsWhenNothingIsExposed(t *testing.T) {
	spec := referenceSpec()
	for index := range spec.Sidecars {
		spec.Sidecars[index].Expose = nil
	}
	for index := range spec.Main {
		spec.Main[index].Expose = nil
	}
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
			name: "no processes",
			edit: func(spec *Spec) { *spec = Spec{} },
			want: ErrInvalidSpec,
		},
		{
			name: "no service",
			edit: func(spec *Spec) {
				spec.Sidecars = nil
				spec.Main = []Process{{
					Name:    "prepare",
					Kind:    RunToCompletion,
					Command: []string{"/app/prepare"},
					Timeout: time.Second,
				}}
			},
			want: ErrInvalidSpec,
		},
		{
			name: "startup budget",
			edit: func(spec *Spec) { spec.Main[1].Probe.StartupTimeout = 20 * time.Second },
			want: ErrStartupBudget,
		},
		{
			name: "missing main name",
			edit: func(spec *Spec) { spec.Main[1].Name = "" },
			want: ErrInvalidSpec,
		},
		{
			name: "duplicate process",
			edit: func(spec *Spec) { spec.Main[1].Name = "egress" },
			want: ErrDuplicateProcess,
		},
		{
			name: "duplicate port",
			edit: func(spec *Spec) { spec.Main[1].Expose = []int{9200} },
			want: ErrDuplicatePort,
		},
		{
			name: "reserved port",
			edit: func(spec *Spec) { spec.Main[1].Expose = []int{ControlPort} },
			want: ErrReservedPort,
		},
		{
			name: "relative executable",
			edit: func(spec *Spec) { spec.Sidecars[0].Command[0] = "usr/bin/egress" },
			want: ErrInvalidSpec,
		},
		{
			name: "invalid kind",
			edit: func(spec *Spec) { spec.Main[2].Kind = "daemon" },
			want: ErrInvalidSpec,
		},
		{
			name: "service timeout",
			edit: func(spec *Spec) { spec.Main[2].Timeout = time.Second },
			want: ErrInvalidSpec,
		},
		{
			name: "job without timeout",
			edit: func(spec *Spec) { spec.Main[0].Timeout = 0 },
			want: ErrInvalidSpec,
		},
		{
			name: "job with probe",
			edit: func(spec *Spec) { spec.Main[0].Probe = HTTPReadinessProbe("/healthz", 8081) },
			want: ErrInvalidSpec,
		},
		{
			name: "job exposing port",
			edit: func(spec *Spec) { spec.Main[0].Expose = []int{8081} },
			want: ErrInvalidSpec,
		},
		{
			name: "invalid failure threshold",
			edit: func(spec *Spec) { spec.Main[1].Probe.FailureThreshold = -1 },
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
				spec.Main[1].User = &ProcessUser{Name: "app"}
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
	spec.Main[1].Env = map[string]string{"LARGE": strings.Repeat("x", MaxEncodedSpecBytes)}
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
				ImageRegistryType: ImageRegistryType("public"),
			},
		},
		{
			SandcampRuntime: referenceImages().SandcampRuntime,
			Sidecars: []SidecarImage{
				{Name: "api", Reference: "api:1", ImageRegistryType: ImageRegistryPersonal},
				{Name: "api", Reference: "api:2", ImageRegistryType: ImageRegistryPersonal},
			},
		},
	}
	for _, images := range tests {
		if _, err := RenderMounts(images); err == nil {
			t.Fatalf("expected invalid image error for %#v", images)
		}
	}
}

func decodeRuntimeSpec(t *testing.T, configuration *ags.CustomConfiguration) (runtimeSpec, []byte) {
	t.Helper()
	if len(configuration.Env) != 1 || *configuration.Env[0].Name != SpecEnvironment {
		t.Fatalf("environment = %#v", configuration.Env)
	}
	raw, err := base64.StdEncoding.DecodeString(*configuration.Env[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded runtimeSpec
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded, raw
}

func referenceImages() ImageSet {
	return ImageSet{
		SandcampRuntime: Image{
			Reference:         "ccr.example.com/team/runtime@sha256:runtime",
			ImageRegistryType: ImageRegistryPersonal,
		},
		Sidecars: []SidecarImage{
			{
				Name:              "egress",
				Reference:         "ccr.example.com/team/egress@sha256:egress",
				ImageRegistryType: ImageRegistryPersonal,
			},
			{
				Name:              "fastapi",
				Reference:         "ccr.example.com/team/fastapi@sha256:fastapi",
				ImageRegistryType: ImageRegistryPersonal,
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
				Probe: &ReadinessProbe{
					Path:           "/healthz",
					Port:           24774,
					StartupTimeout: 3 * time.Second,
				},
			},
			{
				Name:    "fastapi",
				Kind:    Service,
				Command: []string{"/usr/local/bin/python", "/opt/fastapi-proxy/app.py"},
				WorkDir: "/opt/fastapi-proxy",
				User:    &ProcessUser{Name: "app"},
				Expose:  []int{9200},
				Probe: &ReadinessProbe{
					Path:             "/healthz",
					Port:             9200,
					StartupTimeout:   4 * time.Second,
					FailureThreshold: 4,
					SuccessThreshold: 2,
				},
			},
		},
		Main: []Process{
			{
				Name:    "prepare",
				Kind:    RunToCompletion,
				Command: []string{"/app/prepare"},
				Timeout: 2 * time.Second,
			},
			{
				Name:    "app",
				Kind:    Service,
				Command: []string{"/app/server"},
				WorkDir: "/app",
				User:    &ProcessUser{UID: 65532, GID: 65532},
				Expose:  []int{8080},
				Probe: &ReadinessProbe{
					Path:           "/healthz",
					Port:           8080,
					StartupTimeout: 5 * time.Second,
				},
			},
			{
				Name:    "worker",
				Command: []string{"/app/worker"},
			},
		},
	}
}
