// Package testwire renders raw campd declarations for Sandcamp's own
// integration harnesses. It is internal so customer code cannot bypass the
// public ImageSet/RenderStart abstraction.
package testwire

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/csjgg/sandcamp"
	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

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

// Render preserves the raw argv used by low-level mount and signal tests.
func Render(spec sandcamp.Spec) (*ags.CustomConfiguration, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	payload, err := encode(spec)
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

func encode(spec sandcamp.Spec) (string, error) {
	wire := wireSpec{
		Version:   1,
		Processes: make([]wireProcess, 0, len(spec.Sidecars)+1),
	}
	for _, process := range spec.Sidecars {
		wire.Processes = append(wire.Processes, toWireProcess(process, false))
	}
	wire.Processes = append(wire.Processes, toWireProcess(spec.Main, true))
	encoded, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("encode raw test declaration: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encoded), nil
}

func toWireProcess(process sandcamp.Process, main bool) wireProcess {
	name := process.Name
	if main && name == "" {
		name = "main"
	}
	result := wireProcess{
		Name:    name,
		Main:    main,
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
	if process.StartupProbe != nil {
		probe := resolvedProbe(*process.StartupProbe)
		result.StartupProbe = &wireProbe{
			Path:           probe.Path,
			Port:           probe.Port,
			ReadyTimeoutMS: probe.ReadyTimeout.Milliseconds(),
			PeriodMS:       probe.Period.Milliseconds(),
			TimeoutMS:      probe.Timeout.Milliseconds(),
		}
	}
	return result
}

func resolvedProbe(probe sandcamp.StartupProbe) sandcamp.StartupProbe {
	if probe.ReadyTimeout == 0 {
		probe.ReadyTimeout = sandcamp.DefaultReadyTimeout
	}
	if probe.Period == 0 {
		probe.Period = sandcamp.DefaultProbePeriod
	}
	if probe.Timeout == 0 {
		probe.Timeout = sandcamp.DefaultProbeTimeout
	}
	return probe
}

func exposedPorts(spec sandcamp.Spec) []int {
	result := make([]int, 0)
	for _, process := range append(append([]sandcamp.Process(nil), spec.Sidecars...), spec.Main) {
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
