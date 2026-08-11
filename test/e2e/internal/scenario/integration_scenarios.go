package scenario

import (
	"fmt"
	"time"

	"github.com/csjgg/sandcamp"
	"github.com/csjgg/sandcamp/test/e2e/internal/model"
)

const (
	envdPort = 49983
)

func fastAPIRootPair() Scenario {
	return Scenario{
		Name:           "fastapi-root-pair",
		Category:       "real-image",
		Description:    "Start two copies of a Python/FastAPI image as root on distinct ports.",
		ExpectedState:  ExpectedRunning,
		RequiredImages: []string{"fastapi"},
		Fetches: []Fetch{
			{Name: "fastapi-root-a", URL: "http://127.0.0.1:19000/healthz", ExpectedStatus: 200},
			{Name: "fastapi-root-b", URL: "http://127.0.0.1:19001/healthz", ExpectedStatus: 200},
		},
		Build: func(runID, token string) sandcamp.Spec {
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{
					observerProcess(runID, token),
					fastAPIProcess("fastapi-root-a", 19000, runID, token, nil),
					fastAPIProcess("fastapi-root-b", 19001, runID, token, nil),
				},
				Main: []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			first := processes["fastapi-root-a"]
			second := processes["fastapi-root-b"]
			return []Check{
				checkProcessNames(processes, "observer", "fastapi-root-a", "fastapi-root-b", "main"),
				equalCheck("both-fastapi-root", first.UID == 0 && first.GID == 0 && second.UID == 0 && second.GID == 0,
					"both FastAPI sidecars run as root", map[string]any{"first": first, "second": second}),
				equalCheck("fastapi-independent-mounts", first.Namespaces["mnt"] != "" && first.Namespaces["mnt"] != second.Namespaces["mnt"],
					"the two copies have independent mount namespaces", namespaceEvidence(first, second, "mnt")),
			}
		},
	}
}

func fastAPINamedUser() Scenario {
	return Scenario{
		Name:           "fastapi-named-user",
		Category:       "real-image",
		Description:    "Start the Python/FastAPI image using its app passwd entry.",
		ExpectedState:  ExpectedRunning,
		RequiredImages: []string{"fastapi"},
		Fetches: []Fetch{{
			Name: "fastapi-app", URL: "http://127.0.0.1:19002/healthz", ExpectedStatus: 200,
		}},
		Build: func(runID, token string) sandcamp.Spec {
			user := &sandcamp.ProcessUser{Name: "app"}
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{
					observerProcess(runID, token),
					fastAPIProcess("fastapi-root-a", 19002, runID, token, user),
				},
				Main: []sandcamp.Process{mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			process := processes["fastapi-root-a"]
			zeroCaps := process.Status["CapInh"] == "0000000000000000" &&
				process.Status["CapPrm"] == "0000000000000000" &&
				process.Status["CapEff"] == "0000000000000000" &&
				process.Status["CapAmb"] == "0000000000000000"
			return []Check{
				checkProcessNames(processes, "observer", "fastapi-root-a", "main"),
				equalCheck("fastapi-app-identity", process.UID == 65532 && process.GID == 65532,
					"FastAPI app resolves to 65532:65532", map[string]int{"uid": process.UID, "gid": process.GID}),
				equalCheck("fastapi-app-security", len(process.Groups) == 0 && zeroCaps && process.Status["NoNewPrivs"] == "1",
					"FastAPI app has no groups or capabilities and has no_new_privs", process.Status),
			}
		},
	}
}

func egressAllowDeny() Scenario {
	return Scenario{
		Name:           "egress-allow-deny",
		Category:       "network-policy",
		Description:    "Install the real Egress sidecar policy and verify both an allow and a deny decision.",
		ExpectedState:  ExpectedRunning,
		RequiredImages: []string{"egress"},
		Settle:         800 * time.Millisecond,
		Fetches: []Fetch{
			{Name: "egress-policy", URL: "http://127.0.0.1:24774/policy", ExpectedStatus: 200},
			{Name: "egress-allow", URL: "http://example.com/", ExpectedStatus: 200},
			{Name: "egress-deny", URL: "http://example.org/", ExpectedStatus: 0},
		},
		Build: func(runID, token string) sandcamp.Spec {
			egress := process("egress", []string{"/opt/opensandbox-egress/egress"}, 24774, runID, token)
			egress.Env["OPENSANDBOX_EGRESS_MODE"] = "dns"
			egress.Env["OPENSANDBOX_EGRESS_HTTP_ADDR"] = ":24774"
			egress.Env["OPENSANDBOX_EGRESS_RULES"] = `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"},{"action":"allow","target":"*.example.com"}]}`
			egress.Env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
			return sandcamp.Spec{Sidecars: []sandcamp.Process{observerProcess(runID, token), egress}, Main: []sandcamp.Process{mainProcess(runID, token)}}
		},
		Validate: func(snapshot model.Snapshot, observations map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			process := processes["egress"]
			allowed := observations["egress-allow"]
			denied := observations["egress-deny"]
			return []Check{
				checkProcessNames(processes, "observer", "egress", "main"),
				equalCheck("egress-runs-root", process.UID == 0 && process.GID == 0,
					"Egress runs as root so it can install netfilter rules", map[string]int{"uid": process.UID, "gid": process.GID}),
				equalCheck("egress-allow-rule", allowed.Error == "" && allowed.StatusCode == 200,
					"the allow-listed hostname is reachable", allowed),
				equalCheck("egress-deny-rule", denied.StatusCode == 0 && denied.Error != "",
					"the non-allow-listed hostname is blocked", denied),
			}
		},
	}
}

func envdCoexistence() Scenario {
	return Scenario{
		Name:           "envd-coexistence",
		Category:       "external-runtime",
		Description:    "Start a separately mounted envd executable as a campd-managed Main service.",
		ExpectedState:  ExpectedRunning,
		RequiredImages: []string{"envd"},
		Settle:         500 * time.Millisecond,
		Fetches: []Fetch{{
			Name: "envd-health", URL: fmt.Sprintf("http://127.0.0.1:%d/health", envdPort), ExpectedStatus: 204,
		}},
		Build: func(runID, token string) sandcamp.Spec {
			probe := sandcamp.HTTPReadinessProbe("/health", envdPort)
			probe.StartupTimeout = 4 * time.Second
			probe.Period = 200 * time.Millisecond
			probe.Timeout = 500 * time.Millisecond
			envd := sandcamp.Process{
				Name:    "envd",
				Kind:    sandcamp.Service,
				Command: []string{"/mnt/envd-runtime/envd", "-port", fmt.Sprint(envdPort)},
				Env: map[string]string{
					model.RunIDEnvironment: runID,
					model.NameEnvironment:  "envd",
					model.TokenEnvironment: token,
				},
				User:   &sandcamp.ProcessUser{UID: 0, GID: 0},
				Expose: []int{envdPort},
				Probe:  probe,
			}
			return sandcamp.Spec{
				Sidecars: []sandcamp.Process{observerProcess(runID, token)},
				Main:     []sandcamp.Process{envd, mainProcess(runID, token)},
			}
		},
		Validate: func(snapshot model.Snapshot, _ map[string]model.FetchResult) []Check {
			processes := indexProcesses(snapshot.Processes)
			envd := processes["envd"]
			return []Check{
				checkProcessNames(processes, "observer", "envd", "main"),
				equalCheck("envd-managed-by-campd", envd.PPID == 1,
					"envd is a direct child of PID 1 campd", map[string]int{"pid": envd.PID, "ppid": envd.PPID}),
				equalCheck("envd-outside-sidecar-overlay", envd.Namespaces["mnt"] != "" && envd.Namespaces["mnt"] == processes["main"].Namespaces["mnt"],
					"envd stays in the main mount namespace instead of a sandrun namespace", namespaceEvidence(envd, processes["main"], "mnt")),
				equalCheck("envd-runs-root", envd.UID == 0 && envd.GID == 0,
					"envd runs as root", map[string]int{"uid": envd.UID, "gid": envd.GID}),
			}
		},
	}
}

func fastAPIProcess(name string, port int, runID, token string, user *sandcamp.ProcessUser) sandcamp.Process {
	result := process(name,
		[]string{"/usr/local/bin/python", "/opt/fastapi-proxy/app.py"},
		port, runID, token)
	result.WorkDir = "/opt/fastapi-proxy"
	result.User = user
	result.Env["FASTAPI_HOST"] = "127.0.0.1"
	result.Env["FASTAPI_PORT"] = fmt.Sprint(port)
	result.Env["HOME"] = "/home/app"
	result.Env["PATH"] = "/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin"
	result.Env["PYTHONDONTWRITEBYTECODE"] = "1"
	result.Env["PYTHONUNBUFFERED"] = "1"
	result.Env["UPSTREAM_URL"] = "http://127.0.0.1:18081/"
	return result
}

func stringPointers(values ...string) []*string {
	result := make([]*string, 0, len(values))
	for _, value := range values {
		result = append(result, stringPointer(value))
	}
	return result
}

func stringPointer(value string) *string { return &value }
func int64Pointer(value int) *int64 {
	converted := int64(value)
	return &converted
}
