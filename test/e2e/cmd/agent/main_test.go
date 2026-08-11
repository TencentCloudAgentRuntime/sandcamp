package main

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/csjgg/sandcamp/test/e2e/internal/model"
)

func TestEventStoreIsBoundedAndOrdered(t *testing.T) {
	store := &eventStore{}
	for index := 0; index < maxEvents+10; index++ {
		store.add(model.Event{PID: index})
	}
	events := store.copy()
	if len(events) != maxEvents {
		t.Fatalf("event count = %d", len(events))
	}
	if events[0].PID != 10 || events[len(events)-1].PID != maxEvents+9 {
		t.Fatalf("unexpected retained range: %d..%d", events[0].PID, events[len(events)-1].PID)
	}
}

func TestJobOptionsAreBoundedAndRequireFixtureIdentity(t *testing.T) {
	t.Setenv(model.RunIDEnvironment, "run-1")
	t.Setenv(model.NameEnvironment, "prepare")
	t.Setenv(model.TokenEnvironment, "token-1")
	options, err := parseJobOptions([]string{"--delay", "250ms", "--exit-code", "7", "--write", "/tmp/output=value"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Name != "prepare" || options.Delay != 250*time.Millisecond || options.ExitCode != 7 ||
		!reflect.DeepEqual(options.Writes, repeatedFlag{"/tmp/output=value"}) {
		t.Fatalf("options = %#v", options)
	}
	if _, err := parseJobOptions([]string{"--exit-code", "126"}); err == nil {
		t.Fatal("reserved exit code was accepted")
	}
	t.Setenv(model.TokenEnvironment, "")
	if _, err := parseJobOptions(nil); err == nil {
		t.Fatal("job without token was accepted")
	}
}

func TestSelectedEnvironmentCannotLeakArbitraryValues(t *testing.T) {
	selected := selectedEnvironment(map[string]string{
		model.RunIDEnvironment:        "run",
		model.TokenEnvironment:        "fixture-token",
		"HOME":                        "/work/app",
		"TENCENTCLOUD_SECRET_ID":      "must-not-escape",
		"TENCENTCLOUD_SECRET_KEY":     "must-not-escape",
		"ARBITRARY_APPLICATION_VALUE": "must-not-escape",
	})
	if selected[model.RunIDEnvironment] != "run" || selected["HOME"] != "/work/app" {
		t.Fatalf("allow-listed values missing: %#v", selected)
	}
	for _, forbidden := range []string{"TENCENTCLOUD_SECRET_ID", "TENCENTCLOUD_SECRET_KEY", "ARBITRARY_APPLICATION_VALUE"} {
		if _, exists := selected[forbidden]; exists {
			t.Fatalf("forbidden environment %s escaped", forbidden)
		}
	}
	if _, exists := selected[model.TokenEnvironment]; exists {
		t.Fatal("observer token escaped into evidence")
	}
}

func TestIntegerAndNullSeparatedParsing(t *testing.T) {
	if got := integers("1  2\t65532"); !reflect.DeepEqual(got, []int{1, 2, 65532}) {
		t.Fatalf("integers = %v", got)
	}
	path := filepath.Join(t.TempDir(), "cmdline")
	if err := os.WriteFile(path, []byte("agent\x00--name\x00value with spaces\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readNullSeparated(path); !reflect.DeepEqual(got, []string{"agent", "--name", "value with spaces"}) {
		t.Fatalf("cmdline = %#v", got)
	}
}

func TestObserverAuthorizationIsExact(t *testing.T) {
	request := httptest.NewRequest("GET", "http://observer/v1/snapshot", nil)
	if authorized(request, "token") {
		t.Fatal("missing header was accepted")
	}
	request.Header.Set(observerHeader, "token-suffix")
	if authorized(request, "token") {
		t.Fatal("partial token was accepted")
	}
	request.Header.Set(observerHeader, "token")
	if !authorized(request, "token") {
		t.Fatal("exact token was rejected")
	}
}

func TestWriteFixtureFileRejectsUncleanPath(t *testing.T) {
	if err := writeFixtureFile("relative=value"); err == nil {
		t.Fatal("relative path was accepted")
	}
	if err := writeFixtureFile("/tmp/../tmp/value=data"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("unclean path error = %v", err)
	}
}

func TestFetchInputConstructsLoopbackOnlyInsideObserver(t *testing.T) {
	target, err := (fetchInput{Scheme: "http", Host: "loopback", Port: 18082, Path: "/healthz"}).target()
	if err != nil {
		t.Fatal(err)
	}
	if target != "http://127.0.0.1:18082/healthz" {
		t.Fatalf("target = %q", target)
	}
	external, err := (fetchInput{Scheme: "http", Host: "example.org", Path: "/"}).target()
	if err != nil {
		t.Fatal(err)
	}
	if external != "http://example.org/" {
		t.Fatalf("external target = %q", external)
	}
	udp, err := (fetchInput{Scheme: "udp", Host: "loopback", Port: 19082, Path: "/hello%20udp"}).target()
	if err != nil {
		t.Fatal(err)
	}
	if udp != "udp://127.0.0.1:19082/hello%20udp" {
		t.Fatalf("UDP target = %q", udp)
	}
	for _, input := range []fetchInput{
		{Scheme: "file", Host: "loopback", Path: "/etc/passwd"},
		{Scheme: "http", Host: "127.0.0.1:80", Path: "/"},
		{Scheme: "http", Host: "example.com", Port: 70000, Path: "/"},
		{Scheme: "udp", Host: "loopback", Path: "/payload"},
	} {
		if _, err := input.target(); err == nil {
			t.Fatalf("unsafe input accepted: %#v", input)
		}
	}
}

func TestUDPEchoFetch(t *testing.T) {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	errors := make(chan error, 1)
	go serveUDP(listener, "worker-a", func(string, map[string]any) {}, errors)

	result := fetchUDP(context.Background(), "udp://"+listener.LocalAddr().String()+"/hello%20udp")
	if result.Error != "" || result.StatusCode != 200 || result.Body != "worker-a:hello udp" {
		t.Fatalf("UDP fetch = %#v", result)
	}
}

func TestDiagnosticBufferIsBounded(t *testing.T) {
	buffer := &diagnosticBuffer{remaining: 4}
	if written, err := buffer.Write([]byte("abcdef")); err != nil || written != 6 {
		t.Fatalf("Write = %d, %v", written, err)
	}
	if got := buffer.String(); got != "abcd\n[output truncated]" {
		t.Fatalf("buffer = %q", got)
	}
}

func TestMissingDiagnosticCommandIsExplicit(t *testing.T) {
	result := runDiagnosticCommand([]string{"/definitely/not/present"})
	if result.ExitCode != -1 || !strings.Contains(result.Error, "unavailable") {
		t.Fatalf("result = %#v", result)
	}
}

func TestTrackedExecutablesAreExactAndBounded(t *testing.T) {
	tracked, err := parseTrackedExecutables([]string{"nginx=/usr/sbin/nginx", "worker=/opt/bin/worker"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tracked, map[string]string{"/usr/sbin/nginx": "nginx", "/opt/bin/worker": "worker"}) {
		t.Fatalf("tracked = %#v", tracked)
	}
	for _, values := range [][]string{
		{"nginx=usr/sbin/nginx"},
		{"bad name=/bin/app"},
		{"a=/bin/app", "b=/bin/app"},
		{"a=/bin/a", "a=/bin/b"},
	} {
		if _, err := parseTrackedExecutables(values); err == nil {
			t.Fatalf("invalid tracked executable accepted: %#v", values)
		}
	}
}

func TestBootstrapOptionsRequireFixtureIdentityAndAbsoluteCommand(t *testing.T) {
	options, err := parseBootstrapOptions([]string{
		"--run-id", "run-1",
		"--token", "token-1",
		"--listen", "0.0.0.0:18080",
		"--", "/mnt/sandcamp/bin/campd", "--",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.RunID != "run-1" || options.Token != "token-1" ||
		!reflect.DeepEqual(options.Command, []string{"/mnt/sandcamp/bin/campd", "--"}) {
		t.Fatalf("options = %#v", options)
	}
	for _, arguments := range [][]string{
		{"--token", "token-1", "--", "/bin/campd"},
		{"--run-id", "run-1", "--token", "token-1", "--", "campd"},
		{"--run-id", "run-1", "--token", "token-1", "--listen", "invalid", "--", "/bin/campd"},
	} {
		if _, err := parseBootstrapOptions(arguments); err == nil {
			t.Fatalf("invalid bootstrap arguments accepted: %#v", arguments)
		}
	}
}

func TestReplaceEnvironmentRemovesOlderValues(t *testing.T) {
	result := replaceEnvironment([]string{"A=one", "TOKEN=old", "NO_EQUALS"}, map[string]string{
		"TOKEN": "new",
	})
	if strings.Join(result, "|") != "A=one|NO_EQUALS|TOKEN=new" {
		t.Fatalf("environment = %#v", result)
	}
}
