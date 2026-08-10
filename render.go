package sandcamp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

const (
	defaultCampdPath   = "/mnt/sandcamp/bin/campd"
	defaultSandrunPath = "/mnt/sandcamp/bin/sandrun"
	overlayDevicePath  = "/dev/vda"
)

// RenderStart converts image-internal process declarations into the campd
// startup configuration. Image mounts inherit the Tool's StorageMounts, so no
// per-instance MountOptions are required. It performs no cloud API operation.
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

type wireSpec struct {
	Version   int           `json:"version"`
	Processes []wireProcess `json:"processes"`
}

type wireProcess struct {
	Name         string            `json:"name"`
	Main         bool              `json:"main,omitempty"`
	Argv         []string          `json:"argv"`
	Env          map[string]string `json:"env,omitempty"`
	WorkDir      string            `json:"workdir,omitempty"`
	User         *wireUser         `json:"user,omitempty"`
	StartupProbe *wireProbe        `json:"startup_probe,omitempty"`
}

type wireUser struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type wireProbe struct {
	Path           string `json:"path"`
	Port           int    `json:"port"`
	ReadyTimeoutMS int64  `json:"ready_timeout_ms"`
	PeriodMS       int64  `json:"period_ms"`
	TimeoutMS      int64  `json:"timeout_ms"`
}

func encodeSpec(spec Spec, images resolvedImageSet) (string, error) {
	wire := wireSpec{
		Version:   1,
		Processes: make([]wireProcess, 0, len(spec.Sidecars)+1),
	}
	for _, process := range spec.Sidecars {
		wire.Processes = append(
			wire.Processes,
			toWireSidecar(process, images.sidecars[process.Name]),
		)
	}
	wire.Processes = append(wire.Processes, toWireMain(spec.Main))
	encoded, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("encode startup declaration: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encoded), nil
}

func toWireSidecar(process Process, image resolvedImage) wireProcess {
	argv := []string{
		defaultSandrunPath,
		"--rootfs", image.mountPath,
		"--overlay-device", overlayDevicePath,
		"--overlay-id", process.Name,
		"--standard-mounts",
	}
	if process.WorkDir != "" {
		argv = append(argv, "--workdir", process.WorkDir)
	}
	if process.User != nil {
		argv = append(argv, "--user", process.User.Name)
	}
	argv = append(argv, "--")
	argv = append(argv, process.Command...)
	result := wireProcess{
		Name: process.Name,
		Argv: argv,
		Env:  cloneEnvironment(process.Env),
	}
	attachProbe(&result, process.StartupProbe)
	return result
}

func toWireMain(process Process) wireProcess {
	name := process.Name
	if name == "" {
		name = "main"
	}
	result := wireProcess{
		Name:    name,
		Main:    true,
		Argv:    append([]string(nil), process.Command...),
		Env:     cloneEnvironment(process.Env),
		WorkDir: process.WorkDir,
	}
	if process.User != nil {
		result.User = &wireUser{
			UID: process.User.UID,
			GID: process.User.GID,
		}
	}
	attachProbe(&result, process.StartupProbe)
	return result
}

func attachProbe(result *wireProcess, configured *StartupProbe) {
	if configured == nil {
		return
	}
	probe, _ := resolveProbe(*configured)
	result.StartupProbe = &wireProbe{
		Path:           probe.Path,
		Port:           probe.Port,
		ReadyTimeoutMS: probe.ReadyTimeout.Milliseconds(),
		PeriodMS:       probe.Period.Milliseconds(),
		TimeoutMS:      probe.Timeout.Milliseconds(),
	}
}

func exposedPorts(spec Spec) []int {
	result := make([]int, 0)
	for _, process := range append(append([]Process(nil), spec.Sidecars...), spec.Main) {
		result = append(result, process.Expose...)
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
