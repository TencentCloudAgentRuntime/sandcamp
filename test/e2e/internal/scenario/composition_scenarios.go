package scenario

import (
	"fmt"
	"strings"
	"time"

	"github.com/TencentCloudAgentRuntime/sandcamp"
	"github.com/TencentCloudAgentRuntime/sandcamp/test/e2e/internal/model"
)

func sharedBindMount() Scenario {
	const (
		sourceRoot         = "/sandcamp-e2e/auto-created/source"
		targetRoot         = "/sandcamp-e2e/auto-created/target"
		observerTargetFile = targetRoot + "/from-observer.txt"
		workerTargetFile   = targetRoot + "/from-worker.txt"
		mainSourceFile     = sourceRoot + "/from-main.txt"
		deniedTargetFile   = targetRoot + "/readonly-write-must-fail.txt"
	)
	sidecarEvidencePaths := strings.Join([]string{
		observerTargetFile,
		workerTargetFile,
		targetRoot + "/from-main.txt",
		deniedTargetFile,
	}, ",")
	mainEvidencePaths := strings.Join([]string{
		sourceRoot + "/from-observer.txt",
		sourceRoot + "/from-worker.txt",
		mainSourceFile,
		sourceRoot + "/readonly-write-must-fail.txt",
	}, ",")
	readWriteMount := []sandcamp.BindMount{{Source: sourceRoot, Target: targetRoot}}
	readOnlyMount := []sandcamp.BindMount{{Source: sourceRoot, Target: targetRoot, ReadOnly: true}}

	return Scenario{
		Name:          "shared-bind-mount",
		Category:      "filesystem",
		Description:   "Create a missing source and targets, then verify non-root shared writes plus a read-only view.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			observer := observerProcess(runID, token)
			observer.Command = append(observer.Command, "--write", observerTargetFile+"=from-observer")
			observer.Env[model.FilesEnvironment] = sidecarEvidencePaths
			observer.Mounts = readWriteMount

			worker := workerProcess("worker-a", 18082, runID, token, "--write", workerTargetFile+"=from-worker")
			worker.Env[model.FilesEnvironment] = sidecarEvidencePaths
			worker.Mounts = readWriteMount
			worker.User = &sandcamp.ProcessUser{UID: 65532, GID: 65532}

			readOnly := process(
				"worker-b",
				[]string{
					"/bin/sh",
					"-c",
					fmt.Sprintf(
						"if printf 'unexpected' > %s 2>/dev/null; then exit 90; fi; exec %s serve --name worker-b --listen 127.0.0.1:18083",
						deniedTargetFile,
						AgentPath,
					),
				},
				18083,
				runID,
				token,
			)
			readOnly.Env[model.FilesEnvironment] = sidecarEvidencePaths
			readOnly.Mounts = readOnlyMount
			readOnly.User = &sandcamp.ProcessUser{UID: 65532, GID: 65532}

			main := mainProcess(runID, token)
			main.Command = append(main.Command, "--write", mainSourceFile+"=from-main")
			main.Env[model.FilesEnvironment] = mainEvidencePaths
			main.User = &sandcamp.ProcessUser{UID: 65532, GID: 65532}

			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observer, worker, readOnly},
				Main:     []sandcamp.Process{main},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			checks := []Check{
				checkProcessNames(processes, "observer", "worker-a", "worker-b", "main"),
				eventOrderCheck(snapshot.Events, "observer", "worker-a", "worker-b", "main"),
			}
			for _, processName := range []string{"worker-a", "worker-b", "main"} {
				process, found := processes[processName]
				checks = append(checks, equalCheck(
					processName+"-nonroot",
					found && process.UID == 65532 && process.GID == 65532,
					processName+" runs as the declared non-root identity",
					map[string]any{"uid": process.UID, "gid": process.GID},
				))
			}
			expected := []struct {
				file    string
				content string
			}{
				{file: "from-observer.txt", content: "from-observer"},
				{file: "from-worker.txt", content: "from-worker"},
				{file: "from-main.txt", content: "from-main"},
			}
			for _, processName := range []string{"observer", "worker-a", "worker-b", "main"} {
				process, found := processes[processName]
				if !found {
					continue
				}
				root := targetRoot
				if processName == "main" {
					root = sourceRoot
				}
				for _, item := range expected {
					path := root + "/" + item.file
					file := process.Files[path]
					checks = append(checks, equalCheck(
						processName+"-reads-"+strings.TrimSuffix(item.file, ".txt"),
						file.Error == "" && file.Content == item.content,
						processName+" reads the shared file written by its peer",
						file,
					))
				}
			}
			denied := processes["main"].Files[sourceRoot+"/readonly-write-must-fail.txt"]
			checks = append(checks, equalCheck(
				"readonly-bind-blocks-write",
				denied.Error != "",
				"the read-only sidecar cannot create a file in the shared directory",
				denied,
			))
			return checks
		},
	}
}

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
