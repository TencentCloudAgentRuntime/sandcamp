package scenario

import (
	"time"

	"github.com/csjgg/sandcamp"
	"github.com/csjgg/sandcamp/test/e2e/internal/model"
)

func sidecarOnly() Scenario {
	return Scenario{
		Name:          "sidecar-only",
		Category:      "composition",
		Description:   "Reach readiness with no process from the AGS main image.",
		ExpectedState: ExpectedRunning,
		Fetches:       campdReadyFetch(200),
		Build: func(runID, token string) sandcamp.Spec {
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token)}}
		},
		Validate: runningSet("observer"),
	}
}

func multipleMain() Scenario {
	return Scenario{
		Name:          "multiple-main-services",
		Category:      "composition",
		Description:   "Start two ordered services from the same AGS main image RootFS.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token)},
				Main: []sandcamp.Process{
					process("main-a", []string{RuntimeAgentPath, "serve", "--name", "main-a", "--listen", "127.0.0.1:18081"}, 18081, runID, token),
					process("main-b", []string{RuntimeAgentPath, "serve", "--name", "main-b", "--listen", "127.0.0.1:18086"}, 18086, runID, token),
				},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			first := processes["main-a"]
			second := processes["main-b"]
			return []Check{
				checkProcessNames(processes, "observer", "main-a", "main-b"),
				eventOrderCheck(snapshot.Events, "observer", "main-a", "main-b"),
				equalCheck("main-rootfs-shared", first.Namespaces["mnt"] != "" && first.Namespaces["mnt"] == second.Namespaces["mnt"],
					"both main processes use the main image mount namespace", namespaceEvidence(first, second, "mnt")),
			}
		},
	}
}

func mainInitJob() Scenario {
	const evidencePath = "/tmp/sandcamp-main-init"
	return Scenario{
		Name:          "main-init-job",
		Category:      "composition",
		Description:   "Wait for a successful main-image init job before starting the following service.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			main := mainProcess(runID, token)
			main.Env[model.FilesEnvironment] = evidencePath
			prepare := jobProcess("prepare-main", RuntimeAgentPath, runID, token,
				"--delay", "300ms", "--write", evidencePath+"=prepared")
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token)},
				Main:     []sandcamp.Process{prepare, main},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			main := processes["main"]
			file := main.Files[evidencePath]
			return []Check{
				checkProcessNames(processes, "observer", "main"),
				eventBeforeCheck(snapshot.Events, "prepare-main", "job-completed", "main", "started"),
				equalCheck("main-init-output-visible", file.Error == "" && file.Content == "prepared",
					"the following main service sees the init job's filesystem output", file),
			}
		},
	}
}

func sidecarInitJob() Scenario {
	return Scenario{
		Name:          "sidecar-init-job",
		Category:      "composition",
		Description:   "Wait for a Sidecar Image Volume job before starting the next sidecar and main.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			prepare := jobProcess("worker-a", AgentPath, runID, token, "--delay", "300ms")
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{
					observerProcess(runID, token),
					prepare,
					workerProcess("worker-b", 18083, runID, token),
				},
				Main: []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-b", "main"),
				eventBeforeCheck(snapshot.Events, "worker-a", "job-completed", "worker-b", "started"),
				eventOrderCheck(snapshot.Events, "observer", "worker-b", "main"),
			}
		},
	}
}

func serviceWithoutProbe() Scenario {
	return Scenario{
		Name:          "service-without-probe",
		Category:      "probe",
		Description:   "Treat a probe-less service as ready while its PID remains alive.",
		ExpectedState: ExpectedRunning,
		Fetches: []Fetch{
			{Name: "worker-endpoint", URL: "http://127.0.0.1:18082/healthz", ExpectedStatus: 503, ExpectedProcess: "worker-a"},
			{Name: "campd-ready", URL: "http://127.0.0.1:49982/ready", ExpectedStatus: 200},
		},
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token, "--ready-delay", "1h")
			worker.Probe = nil
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token), worker},
				Main:     []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: runningSet("observer", "worker-a", "main"),
	}
}

func jobProcess(name, executable, runID, token string, arguments ...string) sandcamp.Process {
	command := []string{executable, "job", "--name", name}
	command = append(command, arguments...)
	return sandcamp.Process{
		Name:    name,
		Kind:    sandcamp.RunToCompletion,
		Command: command,
		Env: map[string]string{
			model.RunIDEnvironment: runID,
			model.NameEnvironment:  name,
			model.TokenEnvironment: token,
		},
		Timeout: 2 * time.Second,
	}
}

func eventBeforeCheck(events []model.Event, firstProcess, firstKind, secondProcess, secondKind string) Check {
	first := firstEventTime(events, firstProcess, firstKind)
	second := firstEventTime(events, secondProcess, secondKind)
	return equalCheck(
		firstProcess+"-before-"+secondProcess,
		!first.IsZero() && !second.IsZero() && !second.Before(first),
		firstProcess+" completes before "+secondProcess+" starts",
		map[string]string{"first": formatTime(first), "second": formatTime(second)},
	)
}
