// Package testwire renders runtime declarations with test-only filesystem
// options used by Sandcamp's Linux integration harnesses.
package testwire

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/TencentCloudAgentRuntime/sandcamp"
	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

type Bind struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

type SidecarRuntime struct {
	RootFS         string
	OverlayDevice  string
	StandardMounts bool
	Binds          []Bind
}

type runtimeSpec struct {
	Version  int              `json:"version"`
	Sidecars []runtimeSidecar `json:"sidecars"`
	Main     []runtimeMain    `json:"main"`
}

type runtimeProcess struct {
	Name                string               `json:"name"`
	Kind                sandcamp.ProcessKind `json:"kind"`
	Command             []string             `json:"command"`
	Env                 map[string]string    `json:"env,omitempty"`
	WorkDir             string               `json:"workdir,omitempty"`
	ReadinessProbe      *runtimeProbe        `json:"readiness_probe,omitempty"`
	CompletionTimeoutMS int64                `json:"completion_timeout_ms,omitempty"`
}

type runtimeSidecar struct {
	runtimeProcess
	RootFS         string       `json:"rootfs"`
	OverlayDevice  string       `json:"overlay_device,omitempty"`
	StandardMounts bool         `json:"standard_mounts"`
	Binds          []Bind       `json:"binds,omitempty"`
	User           *runtimeUser `json:"user,omitempty"`
}

type runtimeMain struct {
	runtimeProcess
	User *runtimeUser `json:"user,omitempty"`
}

type runtimeUser struct {
	Name *string `json:"name,omitempty"`
	UID  *uint32 `json:"uid,omitempty"`
	GID  *uint32 `json:"gid,omitempty"`
}

type runtimeProbe struct {
	Path             string `json:"path"`
	Port             int    `json:"port"`
	StartupTimeoutMS int64  `json:"startup_timeout_ms"`
	PeriodMS         int64  `json:"period_ms"`
	TimeoutMS        int64  `json:"timeout_ms"`
	FailureThreshold int    `json:"failure_threshold"`
	SuccessThreshold int    `json:"success_threshold"`
}

// Render preserves test-only bind and local-overlay settings while using the
// same runtime declaration contract as the public renderer.
func Render(spec sandcamp.Spec, sidecars map[string]SidecarRuntime) (*ags.CustomConfiguration, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	for _, process := range spec.Sidecars {
		if _, exists := sidecars[process.Name]; !exists {
			return nil, fmt.Errorf("sidecar process %s has no test runtime", process.Name)
		}
	}
	payload, err := encode(spec, sidecars)
	if err != nil {
		return nil, err
	}
	if len(payload) > sandcamp.MaxEncodedSpecBytes {
		return nil, fmt.Errorf(
			"%w: got %d bytes, maximum is %d",
			sandcamp.ErrSpecTooLarge,
			len(payload),
			sandcamp.MaxEncodedSpecBytes,
		)
	}

	ports := exposedPorts(spec)
	var configurationPorts []*ags.PortConfiguration
	if len(ports) > 0 {
		configurationPorts = make([]*ags.PortConfiguration, 0, len(ports))
	}
	for _, port := range ports {
		configurationPorts = append(configurationPorts, &ags.PortConfiguration{
			Name:     stringPointer(fmt.Sprintf("port-%d", port)),
			Port:     int64Pointer(int64(port)),
			Protocol: stringPointer("TCP"),
		})
	}

	return &ags.CustomConfiguration{
		Command: []*string{stringPointer("/mnt/sandcamp/bin/campd")},
		Args:    []*string{stringPointer("--")},
		Env: []*ags.EnvVar{{
			Name:  stringPointer(sandcamp.SpecEnvironment),
			Value: stringPointer(payload),
		}},
		Ports: configurationPorts,
		Probe: &ags.ProbeConfiguration{
			HttpGet: &ags.HttpGetAction{
				Path:   stringPointer("/ready"),
				Port:   int64Pointer(sandcamp.ControlPort),
				Scheme: stringPointer("HTTP"),
			},
			ReadyTimeoutMs:   int64Pointer(30_000),
			ProbePeriodMs:    int64Pointer(1_000),
			ProbeTimeoutMs:   int64Pointer(2_000),
			SuccessThreshold: int64Pointer(1),
			FailureThreshold: int64Pointer(30),
		},
	}, nil
}

func encode(spec sandcamp.Spec, sidecars map[string]SidecarRuntime) (string, error) {
	runtime := runtimeSpec{
		Version:  3,
		Sidecars: make([]runtimeSidecar, 0, len(spec.Sidecars)),
		Main:     make([]runtimeMain, 0, len(spec.Main)),
	}
	for _, process := range spec.Sidecars {
		configured := sidecars[process.Name]
		rendered := runtimeSidecar{
			runtimeProcess: toRuntimeProcess(process),
			RootFS:         configured.RootFS,
			OverlayDevice:  configured.OverlayDevice,
			StandardMounts: configured.StandardMounts,
			Binds:          append([]Bind(nil), configured.Binds...),
		}
		if process.User != nil {
			rendered.User = toRuntimeUser(*process.User)
		}
		runtime.Sidecars = append(runtime.Sidecars, rendered)
	}
	for _, process := range spec.Main {
		rendered := runtimeMain{runtimeProcess: toRuntimeProcess(process)}
		if process.User != nil {
			rendered.User = toRuntimeUser(*process.User)
		}
		runtime.Main = append(runtime.Main, rendered)
	}
	encoded, err := json.Marshal(runtime)
	if err != nil {
		return "", fmt.Errorf("encode test runtime declaration: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encoded), nil
}

func toRuntimeUser(user sandcamp.ProcessUser) *runtimeUser {
	if user.Name != "" {
		name := user.Name
		return &runtimeUser{Name: &name}
	}
	uid, gid := user.UID, user.GID
	return &runtimeUser{UID: &uid, GID: &gid}
}

func toRuntimeProcess(process sandcamp.Process) runtimeProcess {
	kind := process.Kind
	if kind == "" {
		kind = sandcamp.Service
	}
	result := runtimeProcess{
		Name:    process.Name,
		Kind:    kind,
		Command: append([]string(nil), process.Command...),
		Env:     cloneEnvironment(process.Env),
		WorkDir: process.WorkDir,
	}
	if process.Probe != nil {
		probe := resolvedProbe(*process.Probe)
		result.ReadinessProbe = &runtimeProbe{
			Path:             probe.Path,
			Port:             probe.Port,
			StartupTimeoutMS: probe.StartupTimeout.Milliseconds(),
			PeriodMS:         probe.Period.Milliseconds(),
			TimeoutMS:        probe.Timeout.Milliseconds(),
			FailureThreshold: probe.FailureThreshold,
			SuccessThreshold: probe.SuccessThreshold,
		}
	}
	if kind == sandcamp.RunToCompletion {
		result.CompletionTimeoutMS = process.Timeout.Milliseconds()
	}
	return result
}

func resolvedProbe(probe sandcamp.ReadinessProbe) sandcamp.ReadinessProbe {
	if probe.StartupTimeout == 0 {
		probe.StartupTimeout = sandcamp.DefaultStartupTimeout
	}
	if probe.Period == 0 {
		probe.Period = sandcamp.DefaultProbePeriod
	}
	if probe.Timeout == 0 {
		probe.Timeout = sandcamp.DefaultProbeTimeout
	}
	if probe.FailureThreshold == 0 {
		probe.FailureThreshold = sandcamp.DefaultFailureThreshold
	}
	if probe.SuccessThreshold == 0 {
		probe.SuccessThreshold = sandcamp.DefaultSuccessThreshold
	}
	return probe
}

func exposedPorts(spec sandcamp.Spec) []int {
	result := make([]int, 0)
	for _, processes := range [][]sandcamp.Process{spec.Sidecars, spec.Main} {
		for _, process := range processes {
			result = append(result, process.Expose...)
		}
	}
	sort.Ints(result)
	return result
}

func cloneEnvironment(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func stringPointer(value string) *string { return &value }
func int64Pointer(value int64) *int64    { return &value }
