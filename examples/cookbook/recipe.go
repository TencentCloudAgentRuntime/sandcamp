package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/csjgg/sandcamp"
	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

const (
	defaultRegistryType = "personal"

	defaultMainImage    = "registry.example.com/sandcamp/main:replace-me"
	defaultRuntimeImage = "registry.example.com/sandcamp/runtime:replace-me"
	defaultFastAPIImage = "registry.example.com/sandcamp/fastapi:replace-me"
	defaultEgressImage  = "registry.example.com/sandcamp/egress:replace-me"
)

type imageReferences struct {
	registryType string
	main         string
	runtime      string
	fastAPI      string
	egress       string
}

type cookbook struct {
	registryType  string
	mainImage     string
	images        sandcamp.ImageSet
	processes     sandcamp.Spec
	mounts        []*ags.StorageMount
	configuration *ags.CustomConfiguration
}

func defaultImageReferences() imageReferences {
	return imageReferences{
		registryType: defaultRegistryType,
		main:         defaultMainImage,
		runtime:      defaultRuntimeImage,
		fastAPI:      defaultFastAPIImage,
		egress:       defaultEgressImage,
	}
}

func cookbookImageReferences() imageReferences {
	references := defaultImageReferences()
	references.registryType = environmentOrDefault(
		"SANDCAMP_COOKBOOK_REGISTRY_TYPE",
		references.registryType,
	)
	references.main = environmentOrDefault("SANDCAMP_COOKBOOK_MAIN_IMAGE", references.main)
	references.runtime = environmentOrDefault(
		"SANDCAMP_COOKBOOK_RUNTIME_IMAGE",
		references.runtime,
	)
	references.fastAPI = environmentOrDefault(
		"SANDCAMP_COOKBOOK_FASTAPI_IMAGE",
		references.fastAPI,
	)
	references.egress = environmentOrDefault(
		"SANDCAMP_COOKBOOK_EGRESS_IMAGE",
		references.egress,
	)
	return references
}

func environmentOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func buildCookbook(references imageReferences) (*cookbook, error) {
	images := cookbookImages(references)
	processes := cookbookProcesses()
	mounts, err := sandcamp.RenderMounts(images)
	if err != nil {
		return nil, fmt.Errorf("RenderMounts: %w", err)
	}
	configuration, err := sandcamp.RenderStart(images, processes)
	if err != nil {
		return nil, fmt.Errorf("RenderStart: %w", err)
	}
	return &cookbook{
		registryType:  references.registryType,
		mainImage:     references.main,
		images:        images,
		processes:     processes,
		mounts:        mounts,
		configuration: configuration,
	}, nil
}

func cookbookImages(references imageReferences) sandcamp.ImageSet {
	return sandcamp.ImageSet{
		SandcampRuntime: sandcamp.Image{
			Reference:         references.runtime,
			ImageRegistryType: references.registryType,
		},
		Sidecars: []sandcamp.SidecarImage{
			{
				Name:              "egress",
				Reference:         references.egress,
				ImageRegistryType: references.registryType,
			},
			{
				Name:              "fastapi",
				Reference:         references.fastAPI,
				ImageRegistryType: references.registryType,
			},
		},
	}
}

func cookbookProcesses() sandcamp.Spec {
	return sandcamp.Spec{
		Sidecars: []sandcamp.Process{
			{
				Name:    "egress",
				Command: []string{"/opt/opensandbox-egress/egress"},
				Env: map[string]string{
					"OPENSANDBOX_EGRESS_MODE":      "dns",
					"OPENSANDBOX_EGRESS_HTTP_ADDR": ":24774",
					"OPENSANDBOX_EGRESS_RULES":     `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"},{"action":"allow","target":"*.example.com"}]}`,
					"PATH":                         "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				},
				StartupProbe: cookbookProbe("/healthz", 24774),
			},
			{
				Name:    "fastapi",
				Command: []string{"/usr/local/bin/python", "/opt/fastapi-proxy/app.py"},
				WorkDir: "/opt/fastapi-proxy",
				User:    &sandcamp.ProcessUser{Name: "app"},
				Env: map[string]string{
					"FASTAPI_HOST":            "0.0.0.0",
					"FASTAPI_PORT":            "9200",
					"HOME":                    "/home/app",
					"LOGNAME":                 "app",
					"PATH":                    "/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin",
					"PYTHONDONTWRITEBYTECODE": "1",
					"PYTHONUNBUFFERED":        "1",
					"UPSTREAM_URL":            "http://127.0.0.1:8080/",
					"USER":                    "app",
				},
				Expose:       []int{9200},
				StartupProbe: cookbookProbe("/healthz", 9200),
			},
		},
		Main: sandcamp.Process{
			Name:    "main",
			Command: []string{"/usr/local/bin/python", "/opt/sandcamp-validation/app.py"},
			WorkDir: "/opt/sandcamp-validation",
			User:    &sandcamp.ProcessUser{UID: 65532, GID: 65532},
			Env: map[string]string{
				"PATH": "/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin",
			},
			StartupProbe: cookbookProbe("/healthz", 8080),
		},
	}
}

func cookbookProbe(path string, port int) *sandcamp.StartupProbe {
	probe := sandcamp.HTTPStartupProbe(path, port)
	probe.ReadyTimeout = 5 * time.Second
	return probe
}
