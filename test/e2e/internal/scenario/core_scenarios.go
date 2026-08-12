package scenario

import (
	"fmt"
	"strings"
	"time"

	"github.com/TencentCloudAgentRuntime/sandcamp"
	"github.com/TencentCloudAgentRuntime/sandcamp/test/e2e/internal/model"
)

func namedUserAlpine() Scenario {
	return namedUserScenario("named-user-alpine", "worker-a", 18082)
}

func namedUserGlibc() Scenario {
	return namedUserScenario("named-user-glibc", "compat", 18085)
}

func namedUserScenario(name, processName string, port int) Scenario {
	return Scenario{
		Name:          name,
		Category:      "identity",
		Description:   "Resolve app from the sidecar /etc/passwd and remove groups and capabilities.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess(processName, port, runID, token,
				"--write", "/work/app/created.txt=created-by-"+processName)
			worker.WorkDir = "/work/app"
			worker.User = &sandcamp.ProcessUser{Name: "app"}
			worker.Env["HOME"] = "/work/app"
			worker.Env["USER"] = "app"
			worker.Env["LOGNAME"] = "app"
			worker.Env[model.FilesEnvironment] = "/work/app/created.txt"
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token), worker},
				Main:     []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			worker, found := processes[processName]
			checks := []Check{checkProcessNames(processes, "observer", processName, "main")}
			if !found {
				return checks
			}
			zeroCaps := worker.Status["CapInh"] == "0000000000000000" &&
				worker.Status["CapPrm"] == "0000000000000000" &&
				worker.Status["CapEff"] == "0000000000000000" &&
				worker.Status["CapAmb"] == "0000000000000000"
			created := worker.Files["/work/app/created.txt"]
			return append(checks,
				equalCheck("named-user-identity", worker.UID == 65532 && worker.GID == 65532,
					"named user resolves to 65532:65532", map[string]any{"uid": worker.UID, "gid": worker.GID}),
				equalCheck("supplementary-groups-cleared", len(worker.Groups) == 0,
					"named sidecar has no supplementary groups", worker.Groups),
				equalCheck("capabilities-cleared", zeroCaps,
					"inheritable, permitted, effective, and ambient capabilities are zero", worker.Status),
				equalCheck("no-new-privileges", worker.Status["NoNewPrivs"] == "1",
					"no_new_privs is enabled", worker.Status),
				equalCheck("named-user-workdir", worker.Cwd == "/work/app",
					"workdir is applied after pivot_root", map[string]string{"cwd": worker.Cwd}),
				equalCheck("named-user-overlay-write", created.Error == "" && created.Content == "created-by-"+processName && created.UID == 65532 && created.GID == 65532,
					"named user can write its image-owned workdir", created),
			)
		},
	}
}

func sidecarNumericUser() Scenario {
	return Scenario{
		Name:          "sidecar-numeric-user",
		Category:      "identity",
		Description:   "Run a sidecar with explicit numeric IDs that need no passwd entry.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token)
			worker.User = &sandcamp.ProcessUser{UID: 42420, GID: 42421}
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token), worker},
				Main:     []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			worker, found := processes["worker-a"]
			checks := []Check{checkProcessNames(processes, "observer", "worker-a", "main")}
			if !found {
				return checks
			}
			return append(checks, nonRootIdentityChecks("sidecar-numeric", worker, 42420, 42421)...)
		},
	}
}

func mainNumericUser() Scenario {
	return Scenario{
		Name:          "main-numeric-user",
		Category:      "identity",
		Description:   "Drop the main process to a numeric UID/GID while campd remains privileged.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			main := mainProcess(runID, token)
			main.User = &sandcamp.ProcessUser{UID: 65532, GID: 65532}
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token)}, Main: []sandcamp.Process{main}}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			main, found := processes["main"]
			checks := []Check{checkProcessNames(processes, "observer", "main")}
			if !found {
				return checks
			}
			zeroCaps := main.Status["CapInh"] == "0000000000000000" &&
				main.Status["CapPrm"] == "0000000000000000" &&
				main.Status["CapEff"] == "0000000000000000" &&
				main.Status["CapAmb"] == "0000000000000000"
			return append(checks,
				equalCheck("main-numeric-identity", main.UID == 65532 && main.GID == 65532,
					"main runs as 65532:65532", map[string]any{"uid": main.UID, "gid": main.GID}),
				equalCheck("main-groups-cleared", len(main.Groups) == 0,
					"main has no supplementary groups", main.Groups),
				equalCheck("main-capabilities-cleared", zeroCaps,
					"main capability sets are empty after setuid", main.Status),
				equalCheck("main-no-new-privileges", main.Status["NoNewPrivs"] == "1",
					"main has no_new_privs enabled", main.Status),
			)
		},
	}
}

func mainNamedUser() Scenario {
	return Scenario{
		Name:          "main-named-user",
		Category:      "identity",
		Description:   "Resolve a main-image user before init jobs and run the main service with the cached IDs.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			rewritePasswd := sandcamp.Process{
				Name: "rewrite-passwd",
				Kind: sandcamp.RunToCompletion,
				Command: []string{
					"/bin/sh",
					"-c",
					"sed -i 's/^app:x:65532:65532:/app:x:42420:42421:/' /etc/passwd",
				},
				Timeout: 2 * time.Second,
			}
			main := mainProcess(runID, token)
			main.User = &sandcamp.ProcessUser{Name: "app"}
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token)},
				Main:     []sandcamp.Process{rewritePasswd, main},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			main, found := processes["main"]
			checks := []Check{checkProcessNames(processes, "observer", "main")}
			if !found {
				return checks
			}
			return append(checks, nonRootIdentityChecks("main-named", main, 65532, 65532)...)
		},
	}
}

func explicitRootUser() Scenario {
	return Scenario{
		Name:          "explicit-root-user",
		Category:      "identity",
		Description:   "Explicit root identities keep root capabilities and do not enable no_new_privs.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token)
			worker.User = &sandcamp.ProcessUser{UID: 0, GID: 0}
			main := mainProcess(runID, token)
			main.User = &sandcamp.ProcessUser{Name: "root"}
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token), worker},
				Main:     []sandcamp.Process{main},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			checks := []Check{checkProcessNames(processes, "observer", "worker-a", "main")}
			for _, name := range []string{"worker-a", "main"} {
				process, found := processes[name]
				if !found {
					continue
				}
				checks = append(checks,
					equalCheck(name+"-explicit-root", process.UID == 0 && process.GID == 0,
						"explicit root resolves to 0:0", map[string]any{"uid": process.UID, "gid": process.GID}),
					equalCheck(name+"-root-capabilities", process.Status["CapEff"] != "0000000000000000",
						"explicit root retains effective capabilities", process.Status),
					equalCheck(name+"-root-no-new-privileges", process.Status["NoNewPrivs"] == "0",
						"explicit root does not enable no_new_privs", process.Status),
				)
			}
			return checks
		},
	}
}

func nonRootIdentityChecks(prefix string, process model.Process, uid, gid int) []Check {
	zeroCaps := process.Status["CapInh"] == "0000000000000000" &&
		process.Status["CapPrm"] == "0000000000000000" &&
		process.Status["CapEff"] == "0000000000000000" &&
		process.Status["CapAmb"] == "0000000000000000"
	return []Check{
		equalCheck(prefix+"-identity", process.UID == uid && process.GID == gid,
			"process runs with the expected identity", map[string]any{"uid": process.UID, "gid": process.GID}),
		equalCheck(prefix+"-groups-cleared", len(process.Groups) == 0,
			"process has no supplementary groups", process.Groups),
		equalCheck(prefix+"-capabilities-cleared", zeroCaps,
			"non-root capability sets are empty", process.Status),
		equalCheck(prefix+"-no-new-privileges", process.Status["NoNewPrivs"] == "1",
			"non-root process has no_new_privs enabled", process.Status),
	}
}

func delayedReadinessProbe() Scenario {
	return Scenario{
		Name:          "delayed-readiness-probe",
		Category:      "startup",
		Description:   "Retry a failing readiness probe and start main only after it succeeds.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token, "--ready-delay", "1200ms")
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), worker}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			started := firstEventTime(snapshot.Events, "worker-a", "started")
			ready := transitionTime(snapshot.Events, "worker-a", true)
			main := firstEventTime(snapshot.Events, "main", "started")
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "main"),
				equalCheck("probe-retried-until-ready", !started.IsZero() && !ready.IsZero() && ready.Sub(started) >= time.Second,
					"worker readiness is delayed by at least one second", map[string]string{"started": formatTime(started), "ready": formatTime(ready)}),
				equalCheck("main-gated-by-sidecar", !ready.IsZero() && !main.IsZero() && !main.Before(ready),
					"main starts after the delayed sidecar probe succeeds", map[string]string{"worker_ready": formatTime(ready), "main_started": formatTime(main)}),
			}
		},
	}
}

func orderedSidecars() Scenario {
	return Scenario{
		Name:          "ordered-sidecars",
		Category:      "startup",
		Description:   "Start three sidecars serially and gate each successor on the previous probe.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{
					observerProcess(runID, token),
					workerProcess("worker-a", 18082, runID, token, "--ready-delay", "300ms"),
					workerProcess("worker-b", 18083, runID, token, "--ready-delay", "500ms"),
					workerProcess("worker-c", 18084, runID, token, "--ready-delay", "200ms"),
				},
				Main: []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			checks := []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "worker-b", "worker-c", "main"),
				eventOrderCheck(snapshot.Events, "observer", "worker-a", "worker-b", "worker-c", "main"),
			}
			for _, pair := range [][2]string{{"worker-a", "worker-b"}, {"worker-b", "worker-c"}, {"worker-c", "main"}} {
				ready := transitionTime(snapshot.Events, pair[0], true)
				started := firstEventTime(snapshot.Events, pair[1], "started")
				checks = append(checks, equalCheck("gate-"+pair[0]+"-before-"+pair[1], !ready.IsZero() && !started.IsZero() && !started.Before(ready),
					pair[1]+" starts after "+pair[0]+" is ready", map[string]string{"ready": formatTime(ready), "next_started": formatTime(started)}))
			}
			return checks
		},
	}
}

func overlayIsolation() Scenario {
	const evidencePath = "/etc/sandcamp-e2e.txt"
	return Scenario{
		Name:          "overlay-isolation",
		Category:      "filesystem",
		Description:   "Write the same absolute path in two sidecar overlays and keep both lowers immutable.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			observer := observerProcess(runID, token)
			observer.Env[model.FilesEnvironment] = strings.Join([]string{
				"/proc/1/root/mnt/sandcamp-sidecars/worker-a" + evidencePath,
				"/proc/1/root/mnt/sandcamp-sidecars/worker-b" + evidencePath,
			}, ",")
			workerA := workerProcess("worker-a", 18082, runID, token, "--write", evidencePath+"=worker-a")
			workerA.Env[model.FilesEnvironment] = evidencePath
			workerB := workerProcess("worker-b", 18083, runID, token, "--write", evidencePath+"=worker-b")
			workerB.Env[model.FilesEnvironment] = evidencePath
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observer, workerA, workerB}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			workerA := processes["worker-a"]
			workerB := processes["worker-b"]
			observer := processes["observer"]
			fileA := workerA.Files[evidencePath]
			fileB := workerB.Files[evidencePath]
			lowerA := observer.Files["/proc/1/root/mnt/sandcamp-sidecars/worker-a"+evidencePath]
			lowerB := observer.Files["/proc/1/root/mnt/sandcamp-sidecars/worker-b"+evidencePath]
			return []Check{
				checkProcessNames(processes, "observer", "worker-a", "worker-b", "main"),
				equalCheck("independent-overlay-content", fileA.Error == "" && fileA.Content == "worker-a" && fileB.Error == "" && fileB.Content == "worker-b",
					"each sidecar sees its own write at the same path", map[string]any{"worker-a": fileA, "worker-b": fileB}),
				equalCheck("lower-images-immutable", lowerA.Error != "" && lowerB.Error != "",
					"neither immutable lower contains the overlay write", map[string]any{"worker-a-lower": lowerA, "worker-b-lower": lowerB}),
				equalCheck("sidecar-mount-namespaces-distinct", workerA.Namespaces["mnt"] != "" && workerA.Namespaces["mnt"] != workerB.Namespaces["mnt"],
					"sidecars have distinct mount namespaces", namespaceEvidence(workerA, workerB, "mnt")),
			}
		},
	}
}

func argvEnvironmentWorkdir() Scenario {
	const value = "spaces are preserved; unicode=沙箱; marker=--"
	return Scenario{
		Name:          "argv-env-workdir",
		Category:      "process",
		Description:   "Preserve spaces, Unicode, a literal -- marker, environment, and workdir without shell parsing.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token, "--write", "/work/app/argv.txt="+value)
			worker.WorkDir = "/work/app"
			worker.User = &sandcamp.ProcessUser{Name: "app"}
			worker.Env["SANDCAMP_E2E_UNICODE"] = "值 with spaces --"
			worker.Env[model.FilesEnvironment] = "/work/app/argv.txt"
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), worker}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			worker := indexProcesses(snapshot.Processes)["worker-a"]
			file := worker.Files["/work/app/argv.txt"]
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "main"),
				equalCheck("argv-round-trip", contains(worker.Command, "/work/app/argv.txt="+value),
					"the exact argv element reaches exec", worker.Command),
				equalCheck("environment-round-trip", worker.Environment["SANDCAMP_E2E_UNICODE"] == "值 with spaces --",
					"the exact environment value reaches exec", worker.Environment),
				equalCheck("workdir-round-trip", worker.Cwd == "/work/app",
					"the declared workdir is active", worker.Cwd),
				equalCheck("file-content-round-trip", file.Error == "" && file.Content == value,
					"the argv payload is written without shell transformation", file),
			}
		},
	}
}

func loopbackHTTP() Scenario {
	return Scenario{
		Name:          "shared-loopback-http",
		Category:      "network",
		Description:   "Connect from one sidecar to an earlier sidecar over shared loopback.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			workerB := workerProcess("worker-b", 18083, runID, token,
				"--fetch-on-start", "http://127.0.0.1:18082/healthz")
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token), workerProcess("worker-a", 18082, runID, token), workerB},
				Main:     []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			result, found := startupFetch(snapshot.Events, "worker-b", "http://127.0.0.1:18082/healthz")
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "worker-b", "main"),
				equalCheck("sidecar-loopback-connectivity", found && result.StatusCode == 200 && result.Header["X-Sandcamp-E2E-Process"] == "worker-a",
					"worker-b reaches worker-a on 127.0.0.1", result),
			}
		},
	}
}

func publicHTTP() Scenario {
	const target = "http://example.com/"
	return Scenario{
		Name:          "public-http-egress",
		Category:      "network",
		Description:   "Resolve a public hostname and complete an HTTP request from a sidecar.",
		ExpectedState: ExpectedRunning,
		Settle:        2 * time.Second,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token, "--fetch-on-start", target)
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), worker}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			result, found := startupFetch(snapshot.Events, "worker-a", target)
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "main"),
				equalCheck("public-dns-and-http", found && result.Error == "" && result.StatusCode > 0,
					"sidecar resolves example.com and receives an HTTP response", result),
			}
		},
	}
}

func maximumProcessFanout() Scenario {
	return Scenario{
		Name:          "six-process-fanout",
		Category:      "process",
		Description:   "Run the observer, four workload sidecars, and main at the probe-budget boundary.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{
					observerProcess(runID, token),
					workerProcess("worker-a", 18082, runID, token),
					workerProcess("worker-b", 18083, runID, token),
					workerProcess("worker-c", 18084, runID, token),
					workerProcess("compat", 18085, runID, token),
				},
				Main: []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "worker-b", "worker-c", "compat", "main"),
				eventOrderCheck(snapshot.Events, "observer", "worker-a", "worker-b", "worker-c", "compat", "main"),
			}
		},
	}
}

func processGroupChild() Scenario {
	return childScenario("process-group-child", "process-group", true)
}

func sessionChild() Scenario {
	return childScenario("session-child", "session", false)
}

func childScenario(name, mode string, sameGroup bool) Scenario {
	return Scenario{
		Name:          name,
		Category:      "process",
		Description:   "Inspect a descendant created in " + mode + " mode.",
		ExpectedState: ExpectedRunning,
		Settle:        300 * time.Millisecond,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token, "--spawn", mode)
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), worker}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			parent := processes["worker-a"]
			child := processes["worker-a-child"]
			parentGroup := firstStatusInteger(parent.Status["NSpgid"])
			childGroup := firstStatusInteger(child.Status["NSpgid"])
			return []Check{
				checkProcessNames(processes, "observer", "worker-a", "worker-a-child", "main"),
				equalCheck("child-parent-link", child.PPID == parent.PID,
					"child initially belongs to the managed parent", map[string]int{"parent_pid": parent.PID, "child_ppid": child.PPID}),
				equalCheck("child-process-group-topology", (sameGroup && parentGroup == childGroup) || (!sameGroup && parentGroup != childGroup),
					"child process-group membership matches the requested mode", map[string]int{"parent_nspgid": parentGroup, "child_nspgid": childGroup}),
			}
		},
	}
}

func sharedProbeEndpoint() Scenario {
	return Scenario{
		Name:          "shared-probe-endpoint",
		Category:      "boundary",
		Description:   "Confirm that readiness evaluates the configured shared-loopback endpoint, not process identity.",
		ExpectedState: ExpectedRunning,
		Settle:        300 * time.Millisecond,
		Fetches: []Fetch{{
			Name: "actual-worker-b", URL: "http://127.0.0.1:18084/healthz", ExpectedStatus: 503, ExpectedProcess: "worker-b",
		}},
		Build: func(runID, token string) sandcamp.Spec {
			workerA := workerProcess("worker-a", 18082, runID, token, "--extra-listen", "127.0.0.1:18083")
			workerB := workerProcess("worker-b", 18084, runID, token, "--ready-delay", "1h")
			workerB.Probe.Port = 18083
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token), workerA, workerB},
				Main:     []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, observations map[string]model.FetchResult) []Check {
			actual := observations["actual-worker-b"]
			endpointReady := actual.StatusCode == 503 && !firstEventTime(snapshot.Events, "main", "started").IsZero()
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "worker-b", "main"),
				equalCheck("probe-evaluates-configured-endpoint", endpointReady,
					"readiness follows the configured shared-loopback endpoint regardless of responder process", actual),
			}
		},
	}
}

func workerProcess(name string, port int, runID, token string, extra ...string) sandcamp.Process {
	arguments := []string{AgentPath, "serve", "--name", name, "--listen", fmt.Sprintf("127.0.0.1:%d", port)}
	arguments = append(arguments, extra...)
	return process(name, arguments, port, runID, token)
}

func mainProcess(runID, token string) sandcamp.Process {
	return process(
		"main",
		[]string{RuntimeAgentPath, "serve", "--name", "main", "--listen", "127.0.0.1:18081"},
		18081,
		runID,
		token,
	)
}

func firstEventTime(events []model.Event, process, kind string) time.Time {
	for _, event := range events {
		if event.Process == process && event.Kind == kind {
			return event.Time
		}
	}
	return time.Time{}
}

func transitionTime(events []model.Event, process string, ready bool) time.Time {
	for _, event := range events {
		if event.Process != process || event.Kind != "probe-transition" {
			continue
		}
		if value, ok := event.Details["ready"].(bool); ok && value == ready {
			return event.Time
		}
	}
	return time.Time{}
}

func startupFetch(events []model.Event, process, target string) (model.FetchResult, bool) {
	for _, event := range events {
		if event.Process != process || event.Kind != "startup-fetch" {
			continue
		}
		value, ok := event.Details["result"].(map[string]any)
		if !ok || stringValue(value["url"]) != target {
			continue
		}
		result := model.FetchResult{
			URL:        stringValue(value["url"]),
			StatusCode: intValue(value["status_code"]),
			Body:       stringValue(value["body"]),
			Error:      stringValue(value["error"]),
			Header:     make(map[string]string),
		}
		if headers, found := value["header"].(map[string]any); found {
			for key, item := range headers {
				result.Header[key] = stringValue(item)
			}
		}
		return result, true
	}
	return model.FetchResult{}, false
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func firstStatusInteger(value string) int {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return -1
	}
	var result int
	if _, err := fmt.Sscan(fields[0], &result); err != nil {
		return -1
	}
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func intValue(value any) int {
	switch converted := value.(type) {
	case float64:
		return int(converted)
	case int:
		return converted
	default:
		return 0
	}
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}
