package scenario

import (
	"time"

	"github.com/TencentCloudAgentRuntime/sandcamp"
	"github.com/TencentCloudAgentRuntime/sandcamp/test/e2e/internal/model"
	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

func sidecarExitIsolation() Scenario {
	return Scenario{
		Name:          "sidecar-exit-isolation",
		Category:      "lifecycle",
		Description:   "A service sidecar exit makes readiness fail without terminating healthy peers.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{workerProcess("worker-a", 18082, runID, token)},
				Main:     []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Configure: configureExternalObserver,
		Validate:  runningSet("observer", "worker-a", "main"),
		Lifecycle: &Lifecycle{
			Actions: []LifecycleAction{{
				Name:       "exit-sidecar",
				TargetURL:  "http://127.0.0.1:18082/v1/action?action=exit",
				CaptureFor: 800 * time.Millisecond,
				Fetches:    campdReadyFetch(503),
				Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
					processes := indexProcesses(snapshot.Processes)
					return []Check{
						checkProcessNames(processes, "observer", "main"),
						equalCheck("main-survives", processes["main"].PID != 0 && !hasSignal(snapshot.Events, "main", "terminated"),
							"main remains alive and receives no SIGTERM when its peer exits", map[string]any{"main": processes["main"], "signals": signalEvidence(snapshot.Events, "main")}),
					}
				},
			}},
			FinalState: ExpectedRunning,
		},
	}
}

func mainExitIsolation() Scenario {
	return Scenario{
		Name:          "main-exit-isolation",
		Category:      "lifecycle",
		Description:   "A main service exit makes readiness fail without terminating sidecars.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{workerProcess("worker-a", 18082, runID, token)},
				Main:     []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Configure: configureExternalObserver,
		Validate:  runningSet("observer", "worker-a", "main"),
		Lifecycle: &Lifecycle{
			Actions: []LifecycleAction{{
				Name:       "exit-main",
				TargetURL:  "http://127.0.0.1:18081/v1/action?action=exit",
				CaptureFor: 800 * time.Millisecond,
				Fetches:    campdReadyFetch(503),
				Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
					processes := indexProcesses(snapshot.Processes)
					return []Check{
						checkProcessNames(processes, "observer", "worker-a"),
						equalCheck("sidecar-survives", processes["worker-a"].PID != 0 && !hasSignal(snapshot.Events, "worker-a", "terminated"),
							"sidecar remains alive and receives no SIGTERM when main exits", map[string]any{"worker": processes["worker-a"], "signals": signalEvidence(snapshot.Events, "worker-a")}),
					}
				},
			}},
			FinalState: ExpectedRunning,
		},
	}
}

func processGroupShutdown() Scenario {
	return Scenario{
		Name:          "process-group-cleanup",
		Category:      "lifecycle",
		Description:   "Clean up descendants in an exited service's process group without touching peers.",
		ExpectedState: ExpectedRunning,
		Settle:        300 * time.Millisecond,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token, "--spawn", "process-group")
			return sandcamp.Spec{Sidecars: []sandcamp.Process{worker}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Configure: configureExternalObserver,
		Validate:  runningSet("observer", "worker-a", "worker-a-child", "main"),
		Lifecycle: &Lifecycle{
			Actions: []LifecycleAction{{
				Name:       "exit-parent",
				TargetURL:  "http://127.0.0.1:18082/v1/action?action=exit",
				CaptureFor: time.Second,
				Fetches:    campdReadyFetch(503),
				Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
					processes := indexProcesses(snapshot.Processes)
					return []Check{
						checkProcessNames(processes, "observer", "main"),
						equalCheck("process-group-child-signalled", hasSignal(snapshot.Events, "worker-a-child", "terminated"),
							"same-process-group descendant receives SIGTERM", signalEvidence(snapshot.Events, "worker-a-child")),
						equalCheck("peer-survives-group-cleanup", processes["main"].PID != 0,
							"unrelated main service survives process-group cleanup", processes["main"]),
					}
				},
			}},
			FinalState: ExpectedRunning,
		},
	}
}

func setsidProcessGroupBoundary() Scenario {
	return Scenario{
		Name:          "setsid-process-group-boundary",
		Category:      "boundary",
		Description:   "Record setsid descendant behavior outside campd's process-group cleanup guarantee.",
		ExpectedState: ExpectedRunning,
		Settle:        300 * time.Millisecond,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token, "--spawn", "session")
			return sandcamp.Spec{Sidecars: []sandcamp.Process{worker}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Configure: configureExternalObserver,
		Validate:  runningSet("observer", "worker-a", "worker-a-child", "main"),
		Lifecycle: &Lifecycle{
			Actions: []LifecycleAction{{
				Name:       "exit-session-parent",
				TargetURL:  "http://127.0.0.1:18082/v1/action?action=exit",
				CaptureFor: time.Second,
				Fetches:    campdReadyFetch(503),
				Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
					processes := indexProcesses(snapshot.Processes)
					escapedAlive := processes["worker-a-child"].PID != 0
					return []Check{
						{
							Name:    "setsid-descendant-observed",
							Passed:  true,
							Summary: "setsid descendants are observed but are outside campd's process-group cleanup guarantee",
							Evidence: map[string]any{
								"survived": escapedAlive,
								"signals":  signalEvidence(snapshot.Events, "worker-a-child"),
							},
						},
						equalCheck("peer-survives-session-cleanup", processes["main"].PID != 0,
							"unrelated main service survives parent exit", processes["main"]),
					}
				},
			}},
			FinalState: ExpectedRunning,
		},
	}
}

func ignoredTermEscalation() Scenario {
	return Scenario{
		Name:          "residual-group-escalation",
		Category:      "lifecycle",
		Description:   "Escalate from SIGTERM to SIGKILL for a residual process-group child that ignores termination.",
		ExpectedState: ExpectedRunning,
		Settle:        300 * time.Millisecond,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token,
				"--spawn", "process-group", "--child-ignore-term")
			return sandcamp.Spec{Sidecars: []sandcamp.Process{worker}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Configure: configureExternalObserver,
		Validate:  runningSet("observer", "worker-a", "worker-a-child", "main"),
		Lifecycle: &Lifecycle{
			Actions: []LifecycleAction{{
				Name:       "exit-parent-with-stubborn-child",
				TargetURL:  "http://127.0.0.1:18082/v1/action?action=exit",
				CaptureFor: 6500 * time.Millisecond,
				Fetches:    campdReadyFetch(503),
				Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
					processes := indexProcesses(snapshot.Processes)
					return []Check{
						checkProcessNames(processes, "observer", "main"),
						equalCheck("ignored-term-observed", hasSignal(snapshot.Events, "worker-a-child", "terminated"),
							"the child observes SIGTERM before campd escalates", signalEvidence(snapshot.Events, "worker-a-child")),
						equalCheck("stubborn-child-killed", processes["worker-a-child"].PID == 0,
							"the child no longer exists after the five-second grace period", processes["worker-a-child"]),
						equalCheck("peer-survives-escalation", processes["main"].PID != 0,
							"unrelated main service survives SIGKILL escalation", processes["main"]),
					}
				},
			}},
			FinalState: ExpectedRunning,
		},
	}
}

func continuousReadiness() Scenario {
	return Scenario{
		Name:          "continuous-readiness",
		Category:      "probe",
		Description:   "Degrade and recover aggregate readiness without restarting or killing any process.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token)
			worker.Probe.Period = 100 * time.Millisecond
			worker.Probe.FailureThreshold = 2
			worker.Probe.SuccessThreshold = 2
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token), worker},
				Main:     []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: runningSet("observer", "worker-a", "main"),
		Lifecycle: &Lifecycle{
			Actions: []LifecycleAction{
				{
					Name:       "force-unready",
					TargetURL:  "http://127.0.0.1:18082/v1/action?action=unhealthy",
					CaptureFor: 600 * time.Millisecond,
					Fetches: append(campdReadyFetch(503), Fetch{
						Name: "worker-unready", URL: "http://127.0.0.1:18082/healthz", ExpectedStatus: 503, ExpectedProcess: "worker-a",
					}),
					Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
						return []Check{checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "main")}
					},
				},
				{
					Name:       "recover-ready",
					TargetURL:  "http://127.0.0.1:18082/v1/action?action=healthy",
					CaptureFor: 600 * time.Millisecond,
					Fetches: append(campdReadyFetch(200), Fetch{
						Name: "worker-ready", URL: "http://127.0.0.1:18082/healthz", ExpectedStatus: 200, ExpectedProcess: "worker-a",
					}),
					Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
						return []Check{checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "main")}
					},
				},
			},
			FinalState: ExpectedRunning,
		},
	}
}

func campdReadyFetch(status int) []Fetch {
	return []Fetch{{Name: "campd-ready", URL: "http://127.0.0.1:49982/ready", ExpectedStatus: status}}
}

func configureExternalObserver(configuration *ags.CustomConfiguration, runID, token string) {
	configuration.Command = stringPointers(RuntimeAgentPath)
	configuration.Args = stringPointers(
		"bootstrap-observer",
		"--run-id", runID,
		"--token", token,
		"--listen", "0.0.0.0:18080",
		"--",
		"/mnt/sandcamp/bin/campd", "--",
	)
	configuration.Ports = append(configuration.Ports, &ags.PortConfiguration{
		Name: stringPointer("observer"), Port: int64Pointer(ObserverPort), Protocol: stringPointer("TCP"),
	})
}

func runningSet(names ...string) func(model.Snapshot, map[string]model.FetchResult) []Check {
	return func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
		return []Check{checkProcessNames(indexProcesses(snapshot.Processes), names...)}
	}
}

func hasSignal(events []model.Event, process, signal string) bool {
	for _, event := range events {
		if event.Process == process && event.Kind == "signal" && stringValue(event.Details["signal"]) == signal {
			return true
		}
	}
	return false
}

func signalEvidence(events []model.Event, process string) []model.Event {
	result := make([]model.Event, 0)
	for _, event := range events {
		if event.Process == process && event.Kind == "signal" {
			result = append(result, event)
		}
	}
	return result
}
