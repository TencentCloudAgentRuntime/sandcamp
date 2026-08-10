package main

import (
	"fmt"

	"github.com/csjgg/sandcamp"
	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

func Example() {
	registryType := "personal"
	mainImage := "ccr.example.com/team/main@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	images := sandcamp.ImageSet{
		SandcampRuntime: sandcamp.Image{
			Reference:         "ccr.example.com/team/runtime@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			ImageRegistryType: registryType,
		},
		Sidecars: []sandcamp.SidecarImage{{
			Name:              "proxy",
			Reference:         "ccr.example.com/team/proxy@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			ImageRegistryType: registryType,
		}},
	}
	processes := sandcamp.Spec{
		Sidecars: []sandcamp.Process{{
			Name:         "proxy",
			Command:      []string{"/usr/local/bin/proxy"},
			User:         &sandcamp.ProcessUser{Name: "app"},
			Expose:       []int{9200},
			StartupProbe: sandcamp.HTTPStartupProbe("/healthz", 9200),
		}},
		Main: sandcamp.Process{
			Name:    "app",
			Command: []string{"/app/server"},
		},
	}

	mounts, err := sandcamp.RenderMounts(images)
	if err != nil {
		panic(err)
	}
	configuration, err := sandcamp.RenderStart(images, processes)
	if err != nil {
		panic(err)
	}

	toolConfiguration := *configuration
	toolConfiguration.Image = &mainImage
	toolConfiguration.ImageRegistryType = &registryType
	toolRequest := ags.NewCreateSandboxToolRequest()
	toolRequest.StorageMounts = append(toolRequest.StorageMounts, mounts...)
	toolRequest.CustomConfiguration = &toolConfiguration

	startRequest := ags.NewStartSandboxInstanceRequest()
	startRequest.CustomConfiguration = configuration
	// MountOptions 保持 nil，继承 Tool.StorageMounts。

	fmt.Println(len(toolRequest.StorageMounts))
	fmt.Println(*toolRequest.CustomConfiguration.Command[0])
	fmt.Println(startRequest.MountOptions == nil)
	// Output:
	// 2
	// /mnt/sandcamp/bin/campd
	// true
}
