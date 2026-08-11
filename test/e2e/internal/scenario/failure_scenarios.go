package scenario

import (
	"time"

	"github.com/csjgg/sandcamp"
)

func missingNamedUser() Scenario {
	return Scenario{
		Name:          "missing-named-user",
		Category:      "expected-reject",
		Description:   "Reject a sidecar user that is absent from the immutable lower /etc/passwd.",
		ExpectedState: ExpectedStopped,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token)
			worker.User = &sandcamp.ProcessUser{Name: "does-not-exist"}
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), worker}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
	}
}

func missingSidecarExecutable() Scenario {
	return Scenario{
		Name:          "missing-sidecar-executable",
		Category:      "expected-reject",
		Description:   "Reject an executable path that does not exist in the sidecar image.",
		ExpectedState: ExpectedStopped,
		Build: func(runID, token string) sandcamp.Spec {
			worker := process("worker-a", []string{"/does/not/exist"}, 18082, runID, token)
			worker.Probe = nil
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), worker}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
	}
}

func missingMainExecutable() Scenario {
	return Scenario{
		Name:          "missing-main-executable",
		Category:      "expected-reject",
		Description:   "Reject an executable path that does not exist in the main image.",
		ExpectedState: ExpectedStopped,
		Build: func(runID, token string) sandcamp.Spec {
			main := process("main", []string{"/does/not/exist"}, 18081, runID, token)
			main.Probe = nil
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token)}, Main: []sandcamp.Process{main}}
		},
	}
}

func readinessProbeTimeout() Scenario {
	return Scenario{
		Name:          "readiness-probe-timeout",
		Category:      "expected-reject",
		Description:   "Reject initialization when a sidecar never becomes ready within its declared budget.",
		ExpectedState: ExpectedStopped,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token, "--ready-delay", "1h")
			worker.Probe.StartupTimeout = time.Second
			worker.Probe.Period = 100 * time.Millisecond
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), worker}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
	}
}

func initJobFailure() Scenario {
	return Scenario{
		Name:          "init-job-failure",
		Category:      "expected-reject",
		Description:   "Reject initialization when a run-to-completion process exits nonzero.",
		ExpectedState: ExpectedStopped,
		Build: func(runID, token string) sandcamp.Spec {
			job := jobProcess("prepare-main", RuntimeAgentPath, runID, token, "--exit-code", "7")
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token)},
				Main:     []sandcamp.Process{job, mainProcess(runID, token)},
			}
		},
	}
}

func initJobTimeout() Scenario {
	return Scenario{
		Name:          "init-job-timeout",
		Category:      "expected-reject",
		Description:   "Reject initialization and clean up a run-to-completion process that exceeds its timeout.",
		ExpectedState: ExpectedStopped,
		Build: func(runID, token string) sandcamp.Spec {
			job := jobProcess("prepare-main", RuntimeAgentPath, runID, token, "--delay", "5s")
			job.Timeout = 500 * time.Millisecond
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token)},
				Main:     []sandcamp.Process{job, mainProcess(runID, token)},
			}
		},
	}
}

func crashDuringStartup() Scenario {
	return Scenario{
		Name:          "crash-during-startup",
		Category:      "expected-reject",
		Description:   "Reject initialization when a sidecar exits before its readiness probe succeeds.",
		ExpectedState: ExpectedStopped,
		Build: func(runID, token string) sandcamp.Spec {
			worker := workerProcess("worker-a", 18082, runID, token,
				"--ready-delay", "1h", "--exit-after", "300ms", "--exit-code", "42")
			worker.Probe.StartupTimeout = 2 * time.Second
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), worker}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
	}
}
