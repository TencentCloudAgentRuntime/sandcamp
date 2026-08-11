package scenario

import (
	"fmt"
	"sort"
	"time"

	"github.com/csjgg/sandcamp"
	"github.com/csjgg/sandcamp/test/e2e/internal/model"
	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

const (
	ObserverPort     = 18080
	AgentPath        = "/usr/local/bin/sandcamp-e2e-agent"
	RuntimeAgentPath = "/mnt/sandcamp/bin/sandcamp-e2e-agent"
)

type Images struct {
	Runtime      string
	AgentAlpine  string
	AgentGlibc   string
	FastAPI      string
	Egress       string
	Nginx        string
	Envd         string
	Main         string
	RegistryType string
}

type ExpectedState string

const (
	ExpectedRunning   ExpectedState = "RUNNING"
	ExpectedStopped   ExpectedState = "STOPPED"
	ExpectedRestarted ExpectedState = "RUNNING_RESTARTED"
)

type Check struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Summary  string `json:"summary"`
	Evidence any    `json:"evidence,omitempty"`
}

type Scenario struct {
	Name           string
	Category       string
	Description    string
	MainImage      string
	ExpectedState  ExpectedState
	RequiredImages []string
	Settle         time.Duration
	Fetches        []Fetch
	Build          func(runID, token string) sandcamp.Spec
	Configure      func(*ags.CustomConfiguration, string, string)
	Validate       func(model.Snapshot, map[string]model.FetchResult) []Check
	Lifecycle      *Lifecycle
}

type Fetch struct {
	Name            string
	URL             string
	ExpectedStatus  int
	ExpectedProcess string
}

type Lifecycle struct {
	Actions    []LifecycleAction
	FinalState ExpectedState
}

// LifecycleAction changes one running process, waits for campd to observe the
// change, then captures both process state and active HTTP observations.
type LifecycleAction struct {
	Name          string
	TargetURL     string
	SignalProcess string
	Signal        string
	CaptureFor    time.Duration
	Fetches       []Fetch
	Validate      func(model.Snapshot, map[string]model.FetchResult) []Check
}

func CoreImageSet(images Images) sandcamp.ImageSet {
	result := sandcamp.ImageSet{
		SandcampRuntime: sandcamp.Image{
			Reference:         images.Runtime,
			ImageRegistryType: images.RegistryType,
		},
		Sidecars: []sandcamp.SidecarImage{
			{Name: "observer", Reference: images.AgentAlpine, ImageRegistryType: images.RegistryType},
			{Name: "worker-a", Reference: images.AgentAlpine, ImageRegistryType: images.RegistryType},
			{Name: "worker-b", Reference: images.AgentAlpine, ImageRegistryType: images.RegistryType},
			{Name: "worker-c", Reference: images.AgentAlpine, ImageRegistryType: images.RegistryType},
			{Name: "compat", Reference: images.AgentGlibc, ImageRegistryType: images.RegistryType},
		},
	}
	if images.FastAPI != "" {
		result.Sidecars = append(result.Sidecars,
			sandcamp.SidecarImage{Name: "fastapi-root-a", Reference: images.FastAPI, ImageRegistryType: images.RegistryType},
			sandcamp.SidecarImage{Name: "fastapi-root-b", Reference: images.FastAPI, ImageRegistryType: images.RegistryType},
		)
	}
	if images.Egress != "" {
		result.Sidecars = append(result.Sidecars,
			sandcamp.SidecarImage{Name: "egress", Reference: images.Egress, ImageRegistryType: images.RegistryType},
		)
	}
	return result
}

func Core() []Scenario {
	return []Scenario{
		minimalRoot(),
		namedUserAlpine(),
		namedUserGlibc(),
		mainNumericUser(),
		delayedReadinessProbe(),
		orderedSidecars(),
		serviceWithoutProbe(),
		sidecarOnly(),
		multipleMain(),
		mainInitJob(),
		sidecarInitJob(),
		continuousReadiness(),
		overlayIsolation(),
		argvEnvironmentWorkdir(),
		loopbackHTTP(),
		mainToSidecarHTTP(),
		sidecarToMainHTTP(),
		sharedLoopbackUDP(),
		publicHTTP(),
		sidecarNetfilterMutation(),
		maximumProcessFanout(),
		processGroupChild(),
		sessionChild(),
		sidecarExitIsolation(),
		mainExitIsolation(),
		processGroupShutdown(),
		setsidProcessGroupBoundary(),
		ignoredTermEscalation(),
		sharedProbeEndpoint(),
		fastAPIRootPair(),
		fastAPINamedUser(),
		nginxMainReverseProxy(),
		egressAllowDeny(),
		egressNetfilterRules(),
		egressHTTPSAllowDeny(),
		egressWildcardAllow(),
		egressDefaultAllowExplicitDeny(),
		egressRuleCleanup(),
		envdCoexistence(),
		missingNamedUser(),
		missingSidecarExecutable(),
		missingMainExecutable(),
		readinessProbeTimeout(),
		crashDuringStartup(),
		initJobFailure(),
		initJobTimeout(),
		egressInvalidPolicy(),
		egressPortConflict(),
		egressNonRoot(),
	}
}

func ByName(name string) (Scenario, bool) {
	for _, item := range Core() {
		if item.Name == name {
			return item, true
		}
	}
	return Scenario{}, false
}

func minimalRoot() Scenario {
	return Scenario{
		Name:          "minimal-root",
		Category:      "smoke",
		Description:   "A root observer sidecar and a root main process reach readiness.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{
					observerProcess(runID, token),
				},
				Main: []sandcamp.Process{process(
					"main",
					[]string{RuntimeAgentPath, "serve", "--name", "main", "--listen", "127.0.0.1:18081"},
					18081,
					runID,
					token,
				)},
			}
		},
		Validate: validateMinimalRoot,
	}
}

func observerProcess(runID, token string) sandcamp.Process {
	result := process(
		"observer",
		[]string{AgentPath, "observer", "--name", "observer", "--listen", fmt.Sprintf("0.0.0.0:%d", ObserverPort), "--term-delay", "3s"},
		ObserverPort,
		runID,
		token,
	)
	result.Expose = []int{ObserverPort}
	return result
}

func process(name string, command []string, port int, runID, token string) sandcamp.Process {
	probe := sandcamp.HTTPReadinessProbe("/healthz", port)
	probe.StartupTimeout = 4 * time.Second
	probe.Period = 200 * time.Millisecond
	probe.Timeout = 500 * time.Millisecond
	return sandcamp.Process{
		Name:    name,
		Command: command,
		Env: map[string]string{
			model.RunIDEnvironment:        runID,
			model.NameEnvironment:         name,
			model.TokenEnvironment:        token,
			"SANDCAMP_E2E_SCENARIO_VALUE": "declared-" + name,
		},
		Probe: probe,
	}
}

func validateMinimalRoot(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
	checks := make([]Check, 0, 10)
	processes := indexProcesses(snapshot.Processes)
	checks = append(checks, checkProcessNames(processes, "main", "observer"))
	observer, observerOK := processes["observer"]
	main, mainOK := processes["main"]
	if observerOK {
		checks = append(checks,
			equalCheck("observer-root", observer.UID == 0 && observer.GID == 0,
				"observer runs as root", map[string]any{"uid": observer.UID, "gid": observer.GID}),
			equalCheck("observer-overlay", observer.RootFSType == "overlay",
				"observer uses an OverlayFS root", map[string]any{"root_fs_type": observer.RootFSType}),
			equalCheck("observer-env-isolation", observer.Environment[model.ImageEnvironment] == "" && observer.Environment["SANDCAMP_E2E_SCENARIO_VALUE"] == "declared-observer",
				"sidecar receives declared env but not OCI image env", observer.Environment),
		)
	}
	if mainOK {
		checks = append(checks,
			equalCheck("main-root", main.UID == 0 && main.GID == 0,
				"main runs as root", map[string]any{"uid": main.UID, "gid": main.GID}),
			equalCheck("main-image-env", main.Environment[model.ImageEnvironment] == "from-agent-image" && main.Environment["SANDCAMP_E2E_SCENARIO_VALUE"] == "declared-main",
				"main inherits its image env and receives declared env", main.Environment),
		)
	}
	if observerOK && mainOK {
		checks = append(checks,
			equalCheck("shared-pid-namespace", observer.Namespaces["pid"] != "" && observer.Namespaces["pid"] == main.Namespaces["pid"],
				"main and sidecar share the PID namespace", namespaceEvidence(observer, main, "pid")),
			equalCheck("shared-network-namespace", observer.Namespaces["net"] != "" && observer.Namespaces["net"] == main.Namespaces["net"],
				"main and sidecar share the network namespace", namespaceEvidence(observer, main, "net")),
			equalCheck("isolated-mount-namespace", observer.Namespaces["mnt"] != "" && main.Namespaces["mnt"] != "" && observer.Namespaces["mnt"] != main.Namespaces["mnt"],
				"sidecar has a distinct mount namespace", namespaceEvidence(observer, main, "mnt")),
		)
	}
	checks = append(checks, eventOrderCheck(snapshot.Events, "observer", "main"))
	return checks
}

func indexProcesses(processes []model.Process) map[string]model.Process {
	result := make(map[string]model.Process, len(processes))
	for _, process := range processes {
		result[process.Name] = process
	}
	return result
}

func checkProcessNames(processes map[string]model.Process, expected ...string) Check {
	actual := make([]string, 0, len(processes))
	for name := range processes {
		actual = append(actual, name)
	}
	sort.Strings(actual)
	sortedExpected := append([]string(nil), expected...)
	sort.Strings(sortedExpected)
	passed := fmt.Sprint(actual) == fmt.Sprint(sortedExpected)
	return equalCheck("process-set", passed, "all and only expected processes are running", map[string]any{
		"expected": sortedExpected,
		"actual":   actual,
	})
}

func eventOrderCheck(events []model.Event, names ...string) Check {
	started := make(map[string]time.Time)
	for _, event := range events {
		if event.Kind == "started" {
			if _, exists := started[event.Process]; !exists {
				started[event.Process] = event.Time
			}
		}
	}
	passed := true
	previous := time.Time{}
	evidence := make(map[string]string, len(names))
	for _, name := range names {
		value, exists := started[name]
		if !exists || !previous.IsZero() && value.Before(previous) {
			passed = false
		}
		if exists {
			evidence[name] = value.Format(time.RFC3339Nano)
			previous = value
		}
	}
	return equalCheck("startup-order", passed, "process start events follow declaration order", evidence)
}

func namespaceEvidence(left, right model.Process, namespace string) map[string]string {
	return map[string]string{
		left.Name:  left.Namespaces[namespace],
		right.Name: right.Namespaces[namespace],
	}
}

func equalCheck(name string, passed bool, summary string, evidence any) Check {
	if !passed {
		summary = "expected condition was not observed: " + summary
	}
	return Check{Name: name, Passed: passed, Summary: summary, Evidence: evidence}
}
