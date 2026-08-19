package sandcamp

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	ControlPort             = 49982
	MaxExposedPorts         = 8
	MaxEncodedSpecBytes     = 120 * 1024
	AGSReadyTimeout         = 30 * time.Second
	StartupOverheadReserve  = 5 * time.Second
	MaxStartupBudget        = AGSReadyTimeout - StartupOverheadReserve
	DefaultStartupTimeout   = 8 * time.Second
	DefaultProbePeriod      = 500 * time.Millisecond
	DefaultProbeTimeout     = time.Second
	DefaultFailureThreshold = 3
	DefaultSuccessThreshold = 1
	SpecEnvironment         = "SANDCAMP_SPEC"
	AGSLogSidecarPort       = 32000
	AGSAIOSidecarPort       = 57890
)

var (
	ErrInvalidSpec      = errors.New("invalid startup declaration")
	ErrTooManyPorts     = errors.New("too many exposed ports")
	ErrSpecTooLarge     = errors.New("encoded startup declaration is too large")
	ErrStartupBudget    = errors.New("process startup exceeds the AGS readiness budget")
	ErrReservedPort     = errors.New("port is reserved by sandcamp")
	ErrDuplicateProcess = errors.New("process name is duplicated")
	ErrDuplicatePort    = errors.New("exposed port is duplicated")
)

var (
	processNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	environmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	userNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
)

// ProcessKind controls whether campd keeps supervising a process or waits for
// it to complete before starting the next declaration.
type ProcessKind string

const (
	Service         ProcessKind = "service"
	RunToCompletion ProcessKind = "run-to-completion"
)

// Spec declares only the processes needed to make one AGS sandbox start.
// Sidecars are processed first in declaration order, followed by Main. Every
// Main process runs in the AGS main mount namespace, including Tool mounts;
// every Sidecar runs in its matching Image Volume root filesystem.
type Spec struct {
	Sidecars []Process
	Main     []Process
}

// Process is an executable plus its lifecycle and readiness metadata.
// Command[0] and WorkDir are absolute paths visible in that process's root
// filesystem. Main paths may come from the main image or a Tool mount.
type Process struct {
	Name string
	// Kind defaults to Service when omitted.
	Kind    ProcessKind
	Command []string
	Env     map[string]string
	WorkDir string
	User    *ProcessUser
	Expose  []int
	// Mounts bind paths from the sandbox's main Mount Namespace into a
	// Sidecar root filesystem. Main processes cannot configure Mounts.
	Mounts []BindMount

	Probe   *ReadinessProbe // Service only.
	Timeout time.Duration   // RunToCompletion only.
}

// BindMount shares a path from the sandbox's main Mount Namespace with a
// Sidecar. A missing Source and its parents are created as mode 0777 directories.
// A missing Target is created in the Sidecar's writable overlay with the same
// file type as Source; missing directories use mode 0777 and regular files use
// mode 0666. Existing paths are not modified and must have compatible file
// types. Bind mounts do not remap ownership.
type BindMount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// ProcessUser selects a process identity by image user name or explicit
// numeric UID/GID. Name and numeric IDs are mutually exclusive. A Main name
// is resolved from the main image's /etc/passwd; a Sidecar name is resolved
// from that Sidecar image's immutable lower /etc/passwd. Numeric IDs do not
// require a passwd entry.
//
// User == nil inherits the runtime identity (normally root). Every explicitly
// selected identity has empty supplementary groups. Explicit root keeps root
// capabilities; non-root identities have empty capabilities and run with
// no_new_privs enabled.
type ProcessUser struct {
	Name string
	UID  uint32
	GID  uint32
}

// ReadinessProbe is an HTTP GET against the sandbox loopback interface. Its
// first success gates the next declaration; campd continues evaluating it
// after startup so aggregate readiness can change and recover.
type ReadinessProbe struct {
	Path             string
	Port             int
	StartupTimeout   time.Duration
	Period           time.Duration
	Timeout          time.Duration
	FailureThreshold int
	SuccessThreshold int
}

// HTTPReadinessProbe returns a probe with AGS-compatible timing defaults.
func HTTPReadinessProbe(path string, port int) *ReadinessProbe {
	return &ReadinessProbe{Path: path, Port: port}
}

func (spec Spec) Validate() error {
	processCount := len(spec.Sidecars) + len(spec.Main)
	if processCount == 0 {
		return fmt.Errorf("%w: declaration has no processes", ErrInvalidSpec)
	}

	names := make(map[string]struct{}, processCount)
	ports := make(map[int]struct{})
	startupBudget := time.Duration(0)
	serviceCount := 0

	for index, process := range spec.Sidecars {
		if process.Name == "" {
			return fmt.Errorf("%w: sidecars[%d].name is required", ErrInvalidSpec, index)
		}
		if err := validateProcess(process, names, ports, &startupBudget, &serviceCount); err != nil {
			return err
		}
		if err := validateSidecarMounts(process); err != nil {
			return err
		}
	}
	for index, process := range spec.Main {
		if process.Name == "" {
			return fmt.Errorf("%w: main[%d].name is required", ErrInvalidSpec, index)
		}
		if err := validateProcess(process, names, ports, &startupBudget, &serviceCount); err != nil {
			return err
		}
		if len(process.Mounts) != 0 {
			return fmt.Errorf("%w: main process %s cannot configure bind mounts", ErrInvalidSpec, process.Name)
		}
	}
	if serviceCount == 0 {
		return fmt.Errorf("%w: declaration must contain at least one service", ErrInvalidSpec)
	}
	if len(ports) > MaxExposedPorts {
		return fmt.Errorf("%w: got %d, maximum is %d", ErrTooManyPorts, len(ports), MaxExposedPorts)
	}
	if startupBudget > MaxStartupBudget {
		return fmt.Errorf("%w: declared %s, maximum is %s", ErrStartupBudget, startupBudget, MaxStartupBudget)
	}
	return nil
}

var standardSidecarMountTargets = [...]string{
	"/proc",
	"/dev",
	"/sys",
	"/tmp",
	"/run",
	"/etc/resolv.conf",
	"/etc/hosts",
}

func validateSidecarMounts(process Process) error {
	targets := make([]string, 0, len(standardSidecarMountTargets)+len(process.Mounts))
	targets = append(targets, standardSidecarMountTargets[:]...)
	for index, mount := range process.Mounts {
		if !validAbsolutePath(mount.Source) {
			return fmt.Errorf(
				"%w: sidecar process %s mounts[%d].source must be a clean absolute path",
				ErrInvalidSpec,
				process.Name,
				index,
			)
		}
		if !validAbsolutePath(mount.Target) {
			return fmt.Errorf(
				"%w: sidecar process %s mounts[%d].target must be a clean absolute path",
				ErrInvalidSpec,
				process.Name,
				index,
			)
		}
		for _, previous := range targets {
			if mountTargetsOverlap(previous, mount.Target) {
				return fmt.Errorf(
					"%w: sidecar process %s bind target %s overlaps %s",
					ErrInvalidSpec,
					process.Name,
					mount.Target,
					previous,
				)
			}
		}
		targets = append(targets, mount.Target)
	}
	return nil
}

func mountTargetsOverlap(first, second string) bool {
	return first == second ||
		strings.HasPrefix(first, second+"/") ||
		strings.HasPrefix(second, first+"/")
}

func validateProcess(
	process Process,
	names map[string]struct{},
	ports map[int]struct{},
	startupBudget *time.Duration,
	serviceCount *int,
) error {
	if !processNamePattern.MatchString(process.Name) {
		return fmt.Errorf("%w: process %q has an invalid name", ErrInvalidSpec, process.Name)
	}
	if _, exists := names[process.Name]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateProcess, process.Name)
	}
	names[process.Name] = struct{}{}

	if len(process.Command) == 0 || !validAbsolutePath(process.Command[0]) {
		return fmt.Errorf("%w: process %s command must start with a clean absolute path", ErrInvalidSpec, process.Name)
	}
	for _, argument := range process.Command {
		if strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("%w: process %s command contains NUL", ErrInvalidSpec, process.Name)
		}
	}
	if process.WorkDir != "" && !validAbsolutePathAllowRoot(process.WorkDir) {
		return fmt.Errorf("%w: process %s workdir must be a clean absolute path", ErrInvalidSpec, process.Name)
	}
	if process.User != nil {
		if process.User.Name != "" {
			if !userNamePattern.MatchString(process.User.Name) {
				return fmt.Errorf("%w: process %s user name is invalid", ErrInvalidSpec, process.Name)
			}
			if process.User.UID != 0 || process.User.GID != 0 {
				return fmt.Errorf("%w: process %s user name and numeric UID/GID are mutually exclusive", ErrInvalidSpec, process.Name)
			}
		} else if process.User.UID == ^uint32(0) || process.User.GID == ^uint32(0) {
			return fmt.Errorf("%w: process %s user contains the reserved UID/GID value", ErrInvalidSpec, process.Name)
		}
	}
	for key, value := range process.Env {
		if !environmentPattern.MatchString(key) || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%w: process %s has invalid environment variable %q", ErrInvalidSpec, process.Name, key)
		}
		if key == SpecEnvironment {
			return fmt.Errorf("%w: process %s cannot override %s", ErrInvalidSpec, process.Name, SpecEnvironment)
		}
	}
	for _, port := range process.Expose {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%w: process %s exposes %d", ErrInvalidSpec, process.Name, port)
		}
		if reservedPort(port) {
			return fmt.Errorf("%w: %d", ErrReservedPort, port)
		}
		if _, exists := ports[port]; exists {
			return fmt.Errorf("%w: %d", ErrDuplicatePort, port)
		}
		ports[port] = struct{}{}
	}

	kind, err := resolveProcessKind(process.Kind)
	if err != nil {
		return fmt.Errorf("%w: process %s: %v", ErrInvalidSpec, process.Name, err)
	}
	switch kind {
	case Service:
		(*serviceCount)++
		if process.Timeout != 0 {
			return fmt.Errorf("%w: service process %s cannot set timeout", ErrInvalidSpec, process.Name)
		}
		if process.Probe != nil {
			resolved, probeErr := resolveProbe(*process.Probe)
			if probeErr != nil {
				return fmt.Errorf("%w: process %s: %v", ErrInvalidSpec, process.Name, probeErr)
			}
			*startupBudget += resolved.StartupTimeout
		}
	case RunToCompletion:
		if process.Probe != nil {
			return fmt.Errorf("%w: run-to-completion process %s cannot set a readiness probe", ErrInvalidSpec, process.Name)
		}
		if len(process.Expose) != 0 {
			return fmt.Errorf("%w: run-to-completion process %s cannot expose ports", ErrInvalidSpec, process.Name)
		}
		if err := validateCompletionTimeout(process.Timeout); err != nil {
			return fmt.Errorf("%w: process %s: %v", ErrInvalidSpec, process.Name, err)
		}
		*startupBudget += process.Timeout
	}
	return nil
}

func resolveProcessKind(kind ProcessKind) (ProcessKind, error) {
	if kind == "" {
		return Service, nil
	}
	if kind != Service && kind != RunToCompletion {
		return "", fmt.Errorf("process kind %q is invalid", kind)
	}
	return kind, nil
}

func resolveProbe(probe ReadinessProbe) (ReadinessProbe, error) {
	if probe.StartupTimeout == 0 {
		probe.StartupTimeout = DefaultStartupTimeout
	}
	if probe.Period == 0 {
		probe.Period = DefaultProbePeriod
	}
	if probe.Timeout == 0 {
		probe.Timeout = DefaultProbeTimeout
	}
	if probe.FailureThreshold == 0 {
		probe.FailureThreshold = DefaultFailureThreshold
	}
	if probe.SuccessThreshold == 0 {
		probe.SuccessThreshold = DefaultSuccessThreshold
	}
	if !validHTTPOriginForm(probe.Path) {
		return ReadinessProbe{}, fmt.Errorf("probe path must be an encoded HTTP origin-form target")
	}
	if probe.Port < 1 || probe.Port > 65535 {
		return ReadinessProbe{}, fmt.Errorf("probe port is invalid")
	}
	if probe.Port == ControlPort {
		return ReadinessProbe{}, fmt.Errorf("probe port %d is reserved by campd", probe.Port)
	}
	if err := validateProbeDuration("startup timeout", probe.StartupTimeout, time.Second, AGSReadyTimeout); err != nil {
		return ReadinessProbe{}, err
	}
	if err := validateProbeDuration("period", probe.Period, 100*time.Millisecond, AGSReadyTimeout); err != nil {
		return ReadinessProbe{}, err
	}
	if err := validateProbeDuration("timeout", probe.Timeout, 100*time.Millisecond, AGSReadyTimeout); err != nil {
		return ReadinessProbe{}, err
	}
	if probe.Timeout > probe.StartupTimeout {
		return ReadinessProbe{}, fmt.Errorf("probe timeout exceeds startup timeout")
	}
	if probe.FailureThreshold < 1 {
		return ReadinessProbe{}, fmt.Errorf("probe failure threshold must be positive")
	}
	if probe.SuccessThreshold < 1 {
		return ReadinessProbe{}, fmt.Errorf("probe success threshold must be positive")
	}
	if uint64(probe.FailureThreshold) > uint64(^uint32(0)) ||
		uint64(probe.SuccessThreshold) > uint64(^uint32(0)) {
		return ReadinessProbe{}, fmt.Errorf("probe threshold exceeds the runtime limit")
	}
	return probe, nil
}

func validateCompletionTimeout(timeout time.Duration) error {
	if timeout < 100*time.Millisecond || timeout > AGSReadyTimeout || timeout%time.Millisecond != 0 {
		return fmt.Errorf("completion timeout must be a whole millisecond in [%s, %s]", 100*time.Millisecond, AGSReadyTimeout)
	}
	return nil
}

func validHTTPOriginForm(value string) bool {
	if !strings.HasPrefix(value, "/") {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '%' {
			if index+2 >= len(value) || !isHex(value[index+1]) || !isHex(value[index+2]) {
				return false
			}
			index += 2
			continue
		}
		if !isHTTPOriginCharacter(character) {
			return false
		}
	}
	return true
}

func isHTTPOriginCharacter(value byte) bool {
	if value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9' {
		return true
	}
	return strings.ContainsRune("-._~!$&'()*+,;=:@/?", rune(value))
}

func isHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'A' && value <= 'F' || value >= 'a' && value <= 'f'
}

func reservedPort(port int) bool {
	return port == ControlPort || port == AGSLogSidecarPort || port == AGSAIOSidecarPort
}

func validateProbeDuration(label string, value, minimum, maximum time.Duration) error {
	if value < minimum || value > maximum || value%time.Millisecond != 0 {
		return fmt.Errorf("probe %s must be a whole millisecond in [%s, %s]", label, minimum, maximum)
	}
	return nil
}

func validAbsolutePath(value string) bool {
	return value != "/" && validAbsolutePathAllowRoot(value)
}

func validAbsolutePathAllowRoot(value string) bool {
	return value != "" &&
		strings.HasPrefix(value, "/") &&
		!strings.ContainsRune(value, '\x00') &&
		path.Clean(value) == value
}
