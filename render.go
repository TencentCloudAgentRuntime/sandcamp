package sandcamp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

const (
	defaultCampdPath         = "/mnt/sandcamp/bin/campd"
	defaultOverlayDevicePath = "/dev/vda"
	runtimeSpecVersion       = 3
)

// RenderStart converts process declarations into the runtime declaration read
// by campd. Image mounts inherit the Tool's StorageMounts, so no per-instance
// MountOptions are required. It performs no cloud API operation.
func RenderStart(images ImageSet, spec Spec) (*ags.CustomConfiguration, error) {
	resolved, err := resolveImageSet(images)
	if err != nil {
		return nil, err
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	for _, process := range spec.Sidecars {
		if _, exists := resolved.sidecars[process.Name]; !exists {
			return nil, fmt.Errorf("%w: %s", ErrMissingSidecarImage, process.Name)
		}
	}

	payload, err := encodeSpec(spec, resolved)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxEncodedSpecBytes {
		return nil, fmt.Errorf("%w: got %d bytes, maximum is %d", ErrSpecTooLarge, len(payload), MaxEncodedSpecBytes)
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

	configuration := &ags.CustomConfiguration{
		Command: []*string{stringPointer(defaultCampdPath)},
		// AGS instance overrides only replace a Tool's image Args when this
		// slice is non-empty. The inert marker prevents the user's original
		// image CMD from being appended to campd.
		Args: []*string{stringPointer("--")},
		Env: []*ags.EnvVar{{
			Name:  stringPointer(SpecEnvironment),
			Value: stringPointer(payload),
		}},
		Ports: configurationPorts,
		Probe: &ags.ProbeConfiguration{
			HttpGet: &ags.HttpGetAction{
				Path:   stringPointer("/ready"),
				Port:   int64Pointer(ControlPort),
				Scheme: stringPointer("HTTP"),
			},
			ReadyTimeoutMs:   int64Pointer(30_000),
			ProbePeriodMs:    int64Pointer(1_000),
			ProbeTimeoutMs:   int64Pointer(2_000),
			SuccessThreshold: int64Pointer(1),
			FailureThreshold: int64Pointer(30),
		},
	}
	return configuration, nil
}

type runtimeSpec struct {
	Version  int              `json:"version"`
	Sidecars []runtimeSidecar `json:"sidecars"`
	Main     []runtimeMain    `json:"main"`
}

type runtimeProcess struct {
	Name                string            `json:"name"`
	Kind                ProcessKind       `json:"kind"`
	Command             []string          `json:"command"`
	Env                 map[string]string `json:"env,omitempty"`
	WorkDir             string            `json:"workdir,omitempty"`
	ReadinessProbe      *runtimeProbe     `json:"readiness_probe,omitempty"`
	CompletionTimeoutMS int64             `json:"completion_timeout_ms,omitempty"`
}

type runtimeSidecar struct {
	runtimeProcess
	RootFS         string       `json:"rootfs"`
	OverlayDevice  string       `json:"overlay_device,omitempty"`
	StandardMounts bool         `json:"standard_mounts"`
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

func encodeSpec(spec Spec, images resolvedImageSet) (string, error) {
	runtime := runtimeSpec{
		Version:  runtimeSpecVersion,
		Sidecars: make([]runtimeSidecar, 0, len(spec.Sidecars)),
		Main:     make([]runtimeMain, 0, len(spec.Main)),
	}
	for _, process := range spec.Sidecars {
		runtime.Sidecars = append(
			runtime.Sidecars,
			toRuntimeSidecar(process, images.sidecars[process.Name]),
		)
	}
	for _, process := range spec.Main {
		runtime.Main = append(runtime.Main, toRuntimeMain(process))
	}
	encoded, err := json.Marshal(runtime)
	if err != nil {
		return "", fmt.Errorf("encode runtime declaration: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encoded), nil
}

func toRuntimeSidecar(process Process, image resolvedImage) runtimeSidecar {
	result := runtimeSidecar{
		runtimeProcess: toRuntimeProcess(process),
		RootFS:         image.mountPath,
		OverlayDevice:  defaultOverlayDevicePath,
		StandardMounts: true,
	}
	if process.User != nil {
		result.User = toRuntimeUser(*process.User)
	}
	return result
}

func toRuntimeMain(process Process) runtimeMain {
	result := runtimeMain{runtimeProcess: toRuntimeProcess(process)}
	if process.User != nil {
		result.User = toRuntimeUser(*process.User)
	}
	return result
}

func toRuntimeUser(user ProcessUser) *runtimeUser {
	if user.Name != "" {
		name := user.Name
		return &runtimeUser{Name: &name}
	}
	uid, gid := user.UID, user.GID
	return &runtimeUser{UID: &uid, GID: &gid}
}

func toRuntimeProcess(process Process) runtimeProcess {
	kind, _ := resolveProcessKind(process.Kind)
	result := runtimeProcess{
		Name:    process.Name,
		Kind:    kind,
		Command: append([]string(nil), process.Command...),
		Env:     cloneEnvironment(process.Env),
		WorkDir: process.WorkDir,
	}
	if process.Probe != nil {
		probe, _ := resolveProbe(*process.Probe)
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
	if kind == RunToCompletion {
		result.CompletionTimeoutMS = process.Timeout.Milliseconds()
	}
	return result
}

func exposedPorts(spec Spec) []int {
	result := make([]int, 0)
	for _, processes := range [][]Process{spec.Sidecars, spec.Main} {
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
