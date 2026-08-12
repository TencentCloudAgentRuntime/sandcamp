package scenario

import (
	"fmt"
	"strings"
	"time"

	"github.com/TencentCloudAgentRuntime/sandcamp"
	"github.com/TencentCloudAgentRuntime/sandcamp/test/e2e/internal/model"
)

const (
	egressPort            = 24774
	egressExecutable      = "/opt/opensandbox-egress/egress"
	netfilterFixtureChain = "SANDCAMP_E2E"
)

func mainToSidecarHTTP() Scenario {
	const target = "http://127.0.0.1:18082/healthz"
	return Scenario{
		Name:          "main-to-sidecar-http",
		Category:      "network",
		Description:   "Connect from the main image process to a Sidecar Image Volume over shared loopback.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			main := mainProcess(runID, token)
			main.Command = append(main.Command, "--fetch-on-start", target)
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token), workerProcess("worker-a", 18082, runID, token)},
				Main:     []sandcamp.Process{main},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			result, found := startupFetch(snapshot.Events, "main", target)
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "main"),
				equalCheck("main-reaches-sidecar", found && result.StatusCode == 200 && result.Header["X-Sandcamp-E2E-Process"] == "worker-a",
					"main reaches the declared sidecar endpoint on loopback", result),
			}
		},
	}
}

func sidecarToMainHTTP() Scenario {
	return Scenario{
		Name:          "sidecar-to-main-http",
		Category:      "network",
		Description:   "Connect from the observer sidecar to the main image process over shared loopback.",
		ExpectedState: ExpectedRunning,
		Fetches: []Fetch{{
			Name: "main-http", URL: "http://127.0.0.1:18081/healthz", ExpectedStatus: 200, ExpectedProcess: "main",
		}},
		Build: func(runID, token string) sandcamp.Spec {
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token)}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Validate: runningSet("observer", "main"),
	}
}

func sharedLoopbackUDP() Scenario {
	const target = "udp://127.0.0.1:19082/hello%20udp"
	return Scenario{
		Name:          "shared-loopback-udp",
		Category:      "network",
		Description:   "Exchange a UDP datagram between two sidecars over shared loopback.",
		ExpectedState: ExpectedRunning,
		Build: func(runID, token string) sandcamp.Spec {
			udpServer := process("worker-a", []string{
				AgentPath, "serve", "--name", "worker-a", "--listen", "127.0.0.1:18082", "--udp-listen", "127.0.0.1:19082",
			}, 18082, runID, token)
			client := workerProcess("worker-b", 18083, runID, token, "--fetch-on-start", target)
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token), udpServer, client},
				Main:     []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			result, found := startupFetch(snapshot.Events, "worker-b", target)
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "worker-b", "main"),
				equalCheck("sidecar-udp-connectivity", found && result.Error == "" && result.StatusCode == 200 && result.Body == "worker-a:hello udp",
					"worker-b receives the UDP echo from worker-a", result),
			}
		},
	}
}

func sidecarNetfilterMutation() Scenario {
	return Scenario{
		Name:          "sidecar-netfilter-mutation",
		Category:      "network-policy",
		Description:   "Run a root init process in a Sidecar Image Volume and observe its netfilter mutation from another sidecar.",
		ExpectedState: ExpectedRunning,
		Settle:        300 * time.Millisecond,
		Build: func(runID, token string) sandcamp.Spec {
			mutation := sandcamp.Process{
				Name:    "worker-a",
				Kind:    sandcamp.RunToCompletion,
				Command: []string{"/sbin/iptables", "-w", "2", "-t", "filter", "-N", netfilterFixtureChain},
				Env: map[string]string{
					model.RunIDEnvironment: runID,
					model.NameEnvironment:  "worker-a",
					model.TokenEnvironment: token,
				},
				Timeout: 3 * time.Second,
			}
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token), mutation},
				Main:     []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			initial := netfilterText(snapshot.InitialNetfilter)
			current := netfilterText(snapshot.Netfilter)
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "main"),
				equalCheck("netfilter-diagnostics-available", netfilterAvailable(snapshot.Netfilter),
					"the test observer can inspect the shared network namespace", snapshot.Netfilter),
				equalCheck("sidecar-netfilter-visible", !strings.Contains(initial, netfilterFixtureChain) && strings.Contains(current, netfilterFixtureChain),
					"the init process creates a chain visible to the observer", map[string]any{"initial_contains_chain": strings.Contains(initial, netfilterFixtureChain), "current_contains_chain": strings.Contains(current, netfilterFixtureChain)}),
			}
		},
	}
}

func egressNetfilterRules() Scenario {
	return Scenario{
		Name:           "egress-netfilter-rules",
		Category:       "network-policy",
		Description:    "Start the real Egress image and prove that it installs DNS interception rules in the shared network namespace.",
		ExpectedState:  ExpectedRunning,
		RequiredImages: []string{"egress"},
		Settle:         800 * time.Millisecond,
		Fetches: []Fetch{{
			Name: "egress-policy", URL: fmt.Sprintf("http://127.0.0.1:%d/policy", egressPort), ExpectedStatus: 200,
		}},
		Build: func(runID, token string) sandcamp.Spec {
			egress := configuredEgress(runID, token, allowExamplePolicy(), nil)
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), egress}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			delta := netfilterDelta(snapshot)
			joined := strings.Join(delta, "\n")
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "egress", "main"),
				equalCheck("netfilter-diagnostics-available", netfilterAvailable(snapshot.Netfilter),
					"the test observer can inspect the shared network namespace", snapshot.Netfilter),
				equalCheck("egress-installs-dns-redirect", len(delta) > 0 && strings.Contains(joined, "53") && (strings.Contains(joined, "15353") || strings.Contains(strings.ToLower(joined), "redirect")),
					"Egress adds a port-53 redirect relative to the pre-Egress baseline", delta),
			}
		},
	}
}

func egressHTTPSAllowDeny() Scenario {
	return egressDecisionScenario(
		"egress-https-allow-deny",
		"Enforce allow and deny decisions for TLS destinations using the real Egress image.",
		allowExamplePolicy(),
		[]Fetch{
			{Name: "https-allow", URL: "https://example.com/", ExpectedStatus: 200},
			{Name: "https-deny", URL: "https://example.org/", ExpectedStatus: 0},
		},
		"https-allow", "https-deny",
	)
}

func egressWildcardAllow() Scenario {
	return egressDecisionScenario(
		"egress-wildcard-allow",
		"Verify that a wildcard Egress rule admits a subdomain without admitting the parent domain.",
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"*.example.com"}]}`,
		[]Fetch{
			{Name: "wildcard-allow", URL: "https://www.example.com/", ExpectedStatus: 200},
			{Name: "parent-deny", URL: "https://example.com/", ExpectedStatus: 0},
		},
		"wildcard-allow", "parent-deny",
	)
}

func egressDefaultAllowExplicitDeny() Scenario {
	return egressDecisionScenario(
		"egress-default-allow-explicit-deny",
		"Verify an explicit deny rule while unmatched destinations follow default allow.",
		`{"defaultAction":"allow","egress":[{"action":"deny","target":"example.org"}]}`,
		[]Fetch{
			{Name: "default-allow", URL: "https://example.com/", ExpectedStatus: 200},
			{Name: "explicit-deny", URL: "https://example.org/", ExpectedStatus: 0},
		},
		"default-allow", "explicit-deny",
	)
}

func egressInvalidPolicy() Scenario {
	return Scenario{
		Name:           "egress-invalid-policy",
		Category:       "network-policy",
		Description:    "Reject a real Egress process whose policy environment contains malformed JSON.",
		ExpectedState:  ExpectedStopped,
		RequiredImages: []string{"egress"},
		Build: func(runID, token string) sandcamp.Spec {
			egress := configuredEgress(runID, token, `{"defaultAction":`, nil)
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), egress}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
	}
}

func egressPortConflict() Scenario {
	return Scenario{
		Name:           "egress-port-conflict",
		Category:       "network-policy",
		Description:    "Reject Egress startup when an earlier sidecar already owns its HTTP control port.",
		ExpectedState:  ExpectedStopped,
		RequiredImages: []string{"egress"},
		Build: func(runID, token string) sandcamp.Spec {
			conflict := workerProcess("worker-a", egressPort, runID, token)
			egress := configuredEgress(runID, token, allowExamplePolicy(), nil)
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), conflict, egress}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
	}
}

func egressNonRoot() Scenario {
	return Scenario{
		Name:           "egress-nonroot-rejected",
		Category:       "network-policy",
		Description:    "Reject Egress when it is deliberately stripped of the root identity needed for netfilter administration.",
		ExpectedState:  ExpectedStopped,
		RequiredImages: []string{"egress"},
		Build: func(runID, token string) sandcamp.Spec {
			egress := configuredEgress(runID, token, allowExamplePolicy(), &sandcamp.ProcessUser{Name: "nobody"})
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), egress}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
	}
}

func egressRuleCleanup() Scenario {
	var installed []string
	return Scenario{
		Name:           "egress-rule-cleanup",
		Category:       "network-policy",
		Description:    "Terminate Egress and verify that its shared-namespace netfilter mutations do not outlive the process.",
		ExpectedState:  ExpectedRunning,
		RequiredImages: []string{"egress"},
		Settle:         800 * time.Millisecond,
		Fetches: []Fetch{{
			Name: "egress-policy", URL: fmt.Sprintf("http://127.0.0.1:%d/policy", egressPort), ExpectedStatus: 200,
		}},
		Build: func(runID, token string) sandcamp.Spec {
			egress := configuredEgress(runID, token, allowExamplePolicy(), nil)
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), egress}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			installed = egressNetfilterDelta(snapshot)
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "egress", "main"),
				equalCheck("egress-rules-observed-before-stop", len(installed) > 0,
					"Egress-specific netfilter rules are present before termination", installed),
			}
		},
		Lifecycle: &Lifecycle{
			Actions: []LifecycleAction{{
				Name:          "terminate-egress",
				SignalProcess: "egress",
				Signal:        "TERM",
				CaptureFor:    3 * time.Second,
				Fetches: []Fetch{
					{Name: "main-still-serving", URL: "http://127.0.0.1:18081/healthz", ExpectedStatus: 200, ExpectedProcess: "main"},
					{Name: "campd-unready", URL: "http://127.0.0.1:49982/ready", ExpectedStatus: 503},
				},
				Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
					current := netfilterLineSet(snapshot.Netfilter)
					remaining := make([]string, 0)
					for _, line := range installed {
						if _, found := current[line]; found {
							remaining = append(remaining, line)
						}
					}
					return []Check{
						checkProcessNames(indexProcesses(snapshot.Processes), "observer", "main"),
						equalCheck("egress-rules-removed", len(installed) > 0 && len(remaining) == 0,
							"all Egress-specific netfilter rules disappear after termination", map[string]any{"installed": installed, "remaining": remaining}),
					}
				},
			}},
			FinalState: ExpectedRunning,
		},
	}
}

func nginxMainReverseProxy() Scenario {
	return Scenario{
		Name:           "nginx-main-reverse-proxy",
		Category:       "real-image",
		Description:    "Run a pinned Nginx image as Main and proxy traffic to an agent sidecar over shared loopback.",
		MainImage:      "nginx",
		ExpectedState:  ExpectedRunning,
		RequiredImages: []string{"nginx"},
		Fetches: []Fetch{
			{Name: "nginx-health", URL: "http://127.0.0.1:19080/nginx-health", ExpectedStatus: 200},
			{Name: "proxied-worker", URL: "http://127.0.0.1:19080/healthz", ExpectedStatus: 200, ExpectedProcess: "worker-a"},
		},
		Build: func(runID, token string) sandcamp.Spec {
			observer := observerProcess(runID, token)
			observer.Command = append(observer.Command, "--track-executable", "nginx=/usr/sbin/nginx")
			probe := sandcamp.HTTPReadinessProbe("/nginx-health", 19080)
			probe.StartupTimeout = 4 * time.Second
			probe.Period = 200 * time.Millisecond
			probe.Timeout = 500 * time.Millisecond
			nginx := sandcamp.Process{
				Name:    "nginx",
				Kind:    sandcamp.Service,
				Command: []string{"/usr/sbin/nginx", "-g", "daemon off;", "-c", "/etc/nginx/sandcamp-e2e.conf"},
				Env: map[string]string{
					model.RunIDEnvironment: runID,
					model.NameEnvironment:  "nginx",
					model.TokenEnvironment: token,
				},
				User:   &sandcamp.ProcessUser{UID: 0, GID: 0},
				Expose: []int{19080},
				Probe:  probe,
			}
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observer, workerProcess("worker-a", 18082, runID, token)},
				Main:     []sandcamp.Process{nginx},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			var master, worker model.Process
			count := 0
			for _, process := range snapshot.Processes {
				if process.Name != "nginx" {
					continue
				}
				count++
				if process.PPID == 1 {
					master = process
				} else {
					worker = process
				}
			}
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "worker-a", "nginx"),
				equalCheck("nginx-master-worker-tree", count >= 2 && master.PID > 1 && worker.PPID == master.PID,
					"the official Nginx master is managed by campd and owns its worker", map[string]any{"count": count, "master": master, "worker": worker}),
				equalCheck("nginx-master-root", master.PID > 1 && master.UID == 0 && master.GID == 0,
					"the configured Main process starts the Nginx master as root", map[string]int{"uid": master.UID, "gid": master.GID}),
			}
		},
	}
}

func configuredEgress(runID, token, policy string, user *sandcamp.ProcessUser) sandcamp.Process {
	egress := process("egress", []string{egressExecutable}, egressPort, runID, token)
	egress.User = user
	egress.Env["OPENSANDBOX_EGRESS_MODE"] = "dns"
	egress.Env["OPENSANDBOX_EGRESS_HTTP_ADDR"] = fmt.Sprintf(":%d", egressPort)
	egress.Env["OPENSANDBOX_EGRESS_RULES"] = policy
	egress.Env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	return egress
}

func allowExamplePolicy() string {
	return `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"},{"action":"allow","target":"*.example.com"}]}`
}

func egressDecisionScenario(name, description, policy string, fetches []Fetch, allowedName, deniedName string) Scenario {
	return Scenario{
		Name:           name,
		Category:       "network-policy",
		Description:    description,
		ExpectedState:  ExpectedRunning,
		RequiredImages: []string{"egress"},
		Settle:         800 * time.Millisecond,
		Fetches:        fetches,
		Build: func(runID, token string) sandcamp.Spec {
			egress := configuredEgress(runID, token, policy, nil)
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), egress}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Validate: func(snapshot model.Snapshot, observations map[string]model.FetchResult) []Check {
			allowed := observations[allowedName]
			denied := observations[deniedName]
			return []Check{
				checkProcessNames(indexProcesses(snapshot.Processes), "observer", "egress", "main"),
				equalCheck("allowed-destination", allowed.Error == "" && allowed.StatusCode == 200,
					"the allowed destination returns HTTP 200", allowed),
				equalCheck("denied-destination", denied.Error != "" && denied.StatusCode == 0,
					"the denied destination cannot be reached", denied),
			}
		},
	}
}

func netfilterAvailable(snapshot *model.NetfilterSnapshot) bool {
	return snapshot != nil && (snapshot.IPTablesSave.ExitCode == 0 || snapshot.NFTListRules.ExitCode == 0)
}

func netfilterText(snapshot *model.NetfilterSnapshot) string {
	if snapshot == nil {
		return ""
	}
	return snapshot.IPTablesSave.Output + "\n" + snapshot.NFTListRules.Output
}

func netfilterLineSet(snapshot *model.NetfilterSnapshot) map[string]struct{} {
	result := make(map[string]struct{})
	if snapshot == nil {
		return result
	}
	for source, output := range map[string]string{
		"iptables": snapshot.IPTablesSave.Output,
		"nft":      snapshot.NFTListRules.Output,
	} {
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				result[source+":"+line] = struct{}{}
			}
		}
	}
	return result
}

func netfilterDelta(snapshot model.Snapshot) []string {
	initial := netfilterLineSet(snapshot.InitialNetfilter)
	current := netfilterLineSet(snapshot.Netfilter)
	result := make([]string, 0)
	for line := range current {
		if _, found := initial[line]; !found {
			result = append(result, line)
		}
	}
	return result
}

func egressNetfilterDelta(snapshot model.Snapshot) []string {
	result := make([]string, 0)
	for _, line := range netfilterDelta(snapshot) {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "opensandbox") || strings.Contains(lower, "15353") ||
			strings.Contains(lower, "--dport 53") || strings.Contains(lower, "dport 53") {
			result = append(result, line)
		}
	}
	return result
}
