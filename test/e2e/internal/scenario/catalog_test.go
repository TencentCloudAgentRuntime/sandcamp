package scenario

import (
	"strings"
	"testing"

	"github.com/csjgg/sandcamp"
)

func TestCatalogNamesAndRenderedSpecs(t *testing.T) {
	images := Images{
		Runtime:      "registry.example/runtime:test",
		AgentAlpine:  "registry.example/agent:alpine",
		AgentGlibc:   "registry.example/agent:glibc",
		FastAPI:      "registry.example/fastapi:test",
		Egress:       "registry.example/egress:test",
		Envd:         "registry.example/envd:test",
		Main:         "registry.example/main:test",
		RegistryType: "personal",
	}
	seen := make(map[string]struct{})
	for _, item := range Core() {
		t.Run(item.Name, func(t *testing.T) {
			if item.Name == "" || item.Category == "" || item.Description == "" {
				t.Fatalf("incomplete metadata: %#v", item)
			}
			if _, exists := seen[item.Name]; exists {
				t.Fatalf("duplicate scenario %q", item.Name)
			}
			seen[item.Name] = struct{}{}
			spec := item.Build("catalog-run", strings.Repeat("a", 32))
			configuration, err := sandcamp.RenderStart(CoreImageSet(images), spec)
			if err != nil {
				t.Fatalf("RenderStart: %v", err)
			}
			if item.Configure != nil {
				item.Configure(configuration, "catalog-run", strings.Repeat("a", 32))
			}
			if len(configuration.Command) == 0 || configuration.Probe == nil {
				t.Fatalf("configuration lacks required Tool defaults: %#v", configuration)
			}
			if item.ExpectedState == ExpectedRunning && item.Validate == nil {
				t.Fatal("running scenario has no validator")
			}
			if item.Lifecycle != nil {
				if len(item.Lifecycle.Actions) == 0 || item.Lifecycle.FinalState == "" {
					t.Fatalf("incomplete lifecycle: %#v", item.Lifecycle)
				}
				for _, action := range item.Lifecycle.Actions {
					if action.Name == "" || action.TargetURL == "" || action.Validate == nil {
						t.Fatalf("incomplete lifecycle action: %#v", action)
					}
				}
			}
		})
	}
	if len(seen) < 30 {
		t.Fatalf("scenario catalog unexpectedly small: %d", len(seen))
	}
}

func TestCoreImageSetUsesStableMountNames(t *testing.T) {
	images := Images{
		Runtime:      "runtime",
		AgentAlpine:  "alpine",
		AgentGlibc:   "glibc",
		FastAPI:      "fastapi",
		Egress:       "egress",
		RegistryType: "personal",
	}
	mounts, err := sandcamp.RenderMounts(CoreImageSet(images))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sandcamp-runtime",
		"sandcamp-sidecar-observer",
		"sandcamp-sidecar-worker-a",
		"sandcamp-sidecar-worker-b",
		"sandcamp-sidecar-worker-c",
		"sandcamp-sidecar-compat",
		"sandcamp-sidecar-fastapi-root-a",
		"sandcamp-sidecar-fastapi-root-b",
		"sandcamp-sidecar-egress",
	}
	if len(mounts) != len(want) {
		t.Fatalf("mount count = %d, want %d", len(mounts), len(want))
	}
	for index, expected := range want {
		if mounts[index].Name == nil || *mounts[index].Name != expected {
			t.Fatalf("mount[%d] = %#v, want %s", index, mounts[index].Name, expected)
		}
	}
}

func TestBoundaryScenariosRemainExplicit(t *testing.T) {
	want := map[string]bool{
		"shared-probe-endpoint":         false,
		"setsid-process-group-boundary": false,
	}
	for _, item := range Core() {
		if _, expected := want[item.Name]; expected {
			want[item.Name] = item.Category == "boundary"
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("boundary scenario %s is absent or no longer explicit", name)
		}
	}
}
