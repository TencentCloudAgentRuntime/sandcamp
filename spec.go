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
	ControlPort            = 49982
	MaxExposedPorts        = 8
	MaxEncodedSpecBytes    = 120 * 1024
	AGSReadyTimeout        = 30 * time.Second
	StartupOverheadReserve = 5 * time.Second
	MaxStartupProbeBudget  = AGSReadyTimeout - StartupOverheadReserve
	DefaultReadyTimeout    = 8 * time.Second
	DefaultProbePeriod     = 500 * time.Millisecond
	DefaultProbeTimeout    = time.Second
	SpecEnvironment        = "SANDCAMP_SPEC"
	AGSLogSidecarPort      = 32000
	AGSAIOSidecarPort      = 57890
)

var (
	ErrInvalidSpec      = errors.New("invalid startup declaration")
	ErrTooManyPorts     = errors.New("too many exposed ports")
	ErrSpecTooLarge     = errors.New("encoded startup declaration is too large")
	ErrStartupBudget    = errors.New("startup probes exceed the AGS readiness budget")
	ErrReservedPort     = errors.New("port is reserved by sandcamp")
	ErrDuplicateProcess = errors.New("process name is duplicated")
	ErrDuplicatePort    = errors.New("exposed port is duplicated")
)

var (
	processNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	environmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	userNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
)

// Spec declares only the processes needed to make one AGS sandbox start.
// Sidecars are started in declaration order; Main is started last.
type Spec struct {
	Sidecars []Process
	Main     Process
}

// Process is an executable plus its minimum startup metadata. Command[0] and
// WorkDir are absolute paths inside that process's image. RenderStart adds all
// Sandcamp Runtime and filesystem-isolation arguments for sidecars.
type Process struct {
	Name         string
	Command      []string
	Env          map[string]string
	WorkDir      string
	User         *ProcessUser
	Expose       []int
	StartupProbe *StartupProbe
}

// ProcessUser selects a process identity. Main processes use numeric UID/GID.
// Sidecars use Name, resolved from the image's /etc/passwd after sandrun has
// prepared the root filesystem. Supplementary groups are cleared and
// no_new_privs is enabled before exec.
type ProcessUser struct {
	Name string
	UID  uint32
	GID  uint32
}

// StartupProbe is an HTTP GET against loopback. It is evaluated only during
// startup; AGS continues to probe campd's /ready endpoint afterwards.
type StartupProbe struct {
	Path         string
	Port         int
	ReadyTimeout time.Duration
	Period       time.Duration
	Timeout      time.Duration
}

// HTTPStartupProbe returns a probe with AGS-compatible timing defaults.
func HTTPStartupProbe(path string, port int) *StartupProbe {
	return &StartupProbe{Path: path, Port: port}
}

func (spec Spec) Validate() error {
	names := make(map[string]struct{}, len(spec.Sidecars)+1)
	ports := make(map[int]struct{})
	startupBudget := time.Duration(0)

	for index, process := range spec.Sidecars {
		if process.Name == "" {
			return fmt.Errorf("%w: sidecars[%d].name is required", ErrInvalidSpec, index)
		}
		if err := validateProcess(process, process.Name, true, names, ports, &startupBudget); err != nil {
			return err
		}
	}

	mainName := spec.Main.Name
	if mainName == "" {
		mainName = "main"
	}
	if err := validateProcess(spec.Main, mainName, false, names, ports, &startupBudget); err != nil {
		return err
	}
	if len(ports) > MaxExposedPorts {
		return fmt.Errorf("%w: got %d, maximum is %d", ErrTooManyPorts, len(ports), MaxExposedPorts)
	}
	if startupBudget > MaxStartupProbeBudget {
		return fmt.Errorf("%w: declared %s, maximum is %s", ErrStartupBudget, startupBudget, MaxStartupProbeBudget)
	}
	return nil
}

func validateProcess(
	process Process,
	name string,
	sidecar bool,
	names map[string]struct{},
	ports map[int]struct{},
	startupBudget *time.Duration,
) error {
	if !processNamePattern.MatchString(name) {
		return fmt.Errorf("%w: process %q has an invalid name", ErrInvalidSpec, name)
	}
	if _, exists := names[name]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateProcess, name)
	}
	names[name] = struct{}{}

	if len(process.Command) == 0 || !validAbsolutePath(process.Command[0]) {
		return fmt.Errorf("%w: process %s command must start with a clean absolute path", ErrInvalidSpec, name)
	}
	for _, argument := range process.Command {
		if strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("%w: process %s command contains NUL", ErrInvalidSpec, name)
		}
	}
	if process.WorkDir != "" && !validAbsolutePathAllowRoot(process.WorkDir) {
		return fmt.Errorf("%w: process %s workdir must be a clean absolute path", ErrInvalidSpec, name)
	}
	if process.User != nil {
		if sidecar {
			if !userNamePattern.MatchString(process.User.Name) {
				return fmt.Errorf("%w: sidecar process %s user name is invalid", ErrInvalidSpec, name)
			}
			if process.User.UID != 0 || process.User.GID != 0 {
				return fmt.Errorf("%w: sidecar process %s user must use a name only", ErrInvalidSpec, name)
			}
		} else {
			if process.User.Name != "" {
				return fmt.Errorf("%w: main process %s user must use numeric UID/GID", ErrInvalidSpec, name)
			}
			if process.User.UID == ^uint32(0) || process.User.GID == ^uint32(0) {
				return fmt.Errorf("%w: process %s user contains the reserved UID/GID value", ErrInvalidSpec, name)
			}
		}
	}
	for key, value := range process.Env {
		if !environmentPattern.MatchString(key) || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%w: process %s has invalid environment variable %q", ErrInvalidSpec, name, key)
		}
		if key == SpecEnvironment {
			return fmt.Errorf("%w: process %s cannot override %s", ErrInvalidSpec, name, SpecEnvironment)
		}
	}
	for _, port := range process.Expose {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%w: process %s exposes %d", ErrInvalidSpec, name, port)
		}
		if reservedPort(port) {
			return fmt.Errorf("%w: %d", ErrReservedPort, port)
		}
		if _, exists := ports[port]; exists {
			return fmt.Errorf("%w: %d", ErrDuplicatePort, port)
		}
		ports[port] = struct{}{}
	}
	if process.StartupProbe != nil {
		resolved, err := resolveProbe(*process.StartupProbe)
		if err != nil {
			return fmt.Errorf("%w: process %s: %v", ErrInvalidSpec, name, err)
		}
		*startupBudget += resolved.ReadyTimeout
	}
	return nil
}

func resolveProbe(probe StartupProbe) (StartupProbe, error) {
	if probe.ReadyTimeout == 0 {
		probe.ReadyTimeout = DefaultReadyTimeout
	}
	if probe.Period == 0 {
		probe.Period = DefaultProbePeriod
	}
	if probe.Timeout == 0 {
		probe.Timeout = DefaultProbeTimeout
	}
	if !validHTTPOriginForm(probe.Path) {
		return StartupProbe{}, fmt.Errorf("probe path must be an encoded HTTP origin-form target")
	}
	if probe.Port < 1 || probe.Port > 65535 {
		return StartupProbe{}, fmt.Errorf("probe port is invalid")
	}
	if probe.Port == ControlPort {
		return StartupProbe{}, fmt.Errorf("probe port %d is reserved by campd", probe.Port)
	}
	if err := validateProbeDuration("ready timeout", probe.ReadyTimeout, time.Second, AGSReadyTimeout); err != nil {
		return StartupProbe{}, err
	}
	if err := validateProbeDuration("period", probe.Period, 100*time.Millisecond, AGSReadyTimeout); err != nil {
		return StartupProbe{}, err
	}
	if err := validateProbeDuration("timeout", probe.Timeout, 100*time.Millisecond, AGSReadyTimeout); err != nil {
		return StartupProbe{}, err
	}
	if probe.Timeout > probe.ReadyTimeout {
		return StartupProbe{}, fmt.Errorf("probe timeout exceeds ready timeout")
	}
	return probe, nil
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
