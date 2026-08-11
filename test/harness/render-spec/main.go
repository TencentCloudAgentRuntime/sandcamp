package main

import (
	"fmt"
	"os"
	"time"

	"github.com/csjgg/sandcamp"
	"github.com/csjgg/sandcamp/internal/testwire"
)

func main() {
	mainProcess := sandcamp.Process{
		Name: "main",
		Command: []string{
			"/bin/sh",
			"-c",
			"/mnt/sandcamp/bin/sandrun --rootfs /mnt/fastapi --overlay-id main-upstream --workdir /opt/fastapi-proxy --standard-mounts --bind /sandcamp-validation/share /mnt/share --ro-bind /sandcamp-validation/config/probe.txt /etc/sandcamp-validation/probe.txt -- /usr/local/bin/python /opt/fastapi-proxy/upstream.py & wait",
		},
		Env: map[string]string{
			"SANDCAMP_MAIN_PROCESS_ENV": "from-main-process-spec",
		},
		Probe: readinessProbe("/", 8080, 7*time.Second),
	}
	if os.Getenv("SANDCAMP_TEST_NONROOT") == "1" {
		mainProcess.Command = []string{
			"/usr/local/bin/python",
			"-c",
			`import http.server, json, os; status = open("/proc/self/status", encoding="utf-8").read(); selected = {line.split(":", 1)[0]: line.split(":", 1)[1].strip() for line in status.splitlines() if line.split(":", 1)[0] in {"Uid", "Gid", "Groups", "CapEff", "NoNewPrivs"}}; open("/sandcamp-validation/share/nonroot-identity.json", "w", encoding="utf-8").write(json.dumps({"uid": os.getuid(), "gid": os.getgid(), "groups": os.getgroups(), "status": selected})); open("/sandcamp-validation/share/main-created.txt", "w", encoding="utf-8").write("created-by-main"); os.chdir("/tmp"); http.server.ThreadingHTTPServer(("0.0.0.0", 8080), http.server.SimpleHTTPRequestHandler).serve_forever()`,
		}
		mainProcess.User = &sandcamp.ProcessUser{UID: 65532, GID: 65532}
	}

	configuration, err := testwire.Render(sandcamp.Spec{
		Sidecars: []sandcamp.Process{
			{
				Name:    "egress",
				Command: []string{"/opt/opensandbox-egress/egress"},
				Env: map[string]string{
					"OPENSANDBOX_EGRESS_MODE":      "dns",
					"OPENSANDBOX_EGRESS_HTTP_ADDR": ":24774",
					"OPENSANDBOX_EGRESS_RULES":     `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"},{"action":"allow","target":"*.example.com"}]}`,
					"PATH":                         "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				},
				Probe: readinessProbe("/healthz", 24774, 2*time.Second),
			},
			{
				Name: "fastapi",
				Command: []string{
					"/usr/local/bin/python",
					"/opt/fastapi-proxy/app.py",
				},
				WorkDir: "/opt/fastapi-proxy",
				Env: map[string]string{
					"FASTAPI_PORT":            "9200",
					"HOME":                    "/root",
					"LANG":                    "C.UTF-8",
					"PATH":                    "/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin",
					"PYTHONDONTWRITEBYTECODE": "1",
					"PYTHONUNBUFFERED":        "1",
					"UPSTREAM_URL":            "http://127.0.0.1:8080/",
					"SANDCAMP_SIDECAR_ENV":    "from-process-spec",
				},
				Expose: []int{9200},
				Probe:  readinessProbe("/healthz", 9200, 16*time.Second),
			},
		},
		Main: []sandcamp.Process{mainProcess},
	}, map[string]testwire.SidecarRuntime{
		"egress": {
			RootFS:         "/mnt/egress",
			StandardMounts: true,
		},
		"fastapi": {
			RootFS:         "/mnt/fastapi",
			StandardMounts: true,
			Binds: []testwire.Bind{
				{
					Source: "/sandcamp-validation/share",
					Target: "/mnt/share",
				},
				{
					Source:   "/sandcamp-validation/config/probe.txt",
					Target:   "/etc/sandcamp-validation/probe.txt",
					ReadOnly: true,
				},
			},
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, variable := range configuration.Env {
		if variable.Name != nil && *variable.Name == sandcamp.SpecEnvironment && variable.Value != nil {
			fmt.Print(*variable.Value)
			return
		}
	}
	fmt.Fprintln(os.Stderr, "rendered configuration has no SANDCAMP_SPEC")
	os.Exit(1)
}

func readinessProbe(path string, port int, timeout time.Duration) *sandcamp.ReadinessProbe {
	return &sandcamp.ReadinessProbe{
		Path:           path,
		Port:           port,
		StartupTimeout: timeout,
	}
}
