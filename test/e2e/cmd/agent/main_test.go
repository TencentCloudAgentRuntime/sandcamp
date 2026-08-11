package main

import (
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
	for _, input := range []fetchInput{
		{Scheme: "file", Host: "loopback", Path: "/etc/passwd"},
		{Scheme: "http", Host: "127.0.0.1:80", Path: "/"},
		{Scheme: "http", Host: "example.com", Port: 70000, Path: "/"},
	} {
		if _, err := input.target(); err == nil {
			t.Fatalf("unsafe input accepted: %#v", input)
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
