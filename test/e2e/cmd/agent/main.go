package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/csjgg/sandcamp/test/e2e/internal/model"
)

const (
	maxBodyBytes    = 64 << 10
	maxFileBytes    = 32 << 10
	maxEvents       = 2048
	observerHeader  = "X-Sandcamp-E2E-Token"
	defaultObserver = "http://127.0.0.1:18080"
)

type repeatedFlag []string

func (values *repeatedFlag) String() string { return strings.Join(*values, ",") }
func (values *repeatedFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type serveOptions struct {
	Observer        bool
	Name            string
	Listen          string
	ExtraListen     repeatedFlag
	ProbePath       string
	ObserverURL     string
	ReadyDelay      time.Duration
	UnhealthyAfter  time.Duration
	ExitAfter       time.Duration
	ExitCode        int
	TermDelay       time.Duration
	IgnoreTerm      bool
	Spawn           string
	ChildIgnoreTerm bool
	Writes          repeatedFlag
	FetchOnStart    repeatedFlag
}

type bootstrapOptions struct {
	RunID   string
	Token   string
	Listen  string
	Command []string
}

type jobOptions struct {
	Name        string
	ObserverURL string
	Delay       time.Duration
	ExitCode    int
	Writes      repeatedFlag
}

type eventStore struct {
	mu     sync.Mutex
	events []model.Event
}

func (store *eventStore) add(event model.Event) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.events) == maxEvents {
		copy(store.events, store.events[1:])
		store.events = store.events[:maxEvents-1]
	}
	store.events = append(store.events, event)
}

func (store *eventStore) copy() []model.Event {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]model.Event(nil), store.events...)
}

func main() {
	if len(os.Args) < 2 {
		fatal("expected a subcommand: bootstrap-observer, observer, serve, child, or job")
	}
	var err error
	switch os.Args[1] {
	case "bootstrap-observer":
		var options bootstrapOptions
		options, err = parseBootstrapOptions(os.Args[2:])
		if err == nil {
			err = runBootstrap(options)
		}
	case "observer":
		err = runServer(parseServeOptions(os.Args[2:], true))
	case "serve":
		err = runServer(parseServeOptions(os.Args[2:], false))
	case "child":
		err = runChild(os.Args[2:])
	case "job":
		var options jobOptions
		options, err = parseJobOptions(os.Args[2:])
		if err == nil {
			os.Exit(runJob(options))
		}
	case "version", "--version":
		fmt.Println("sandcamp-e2e-agent v1")
		return
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fatal(err.Error())
	}
}

func parseJobOptions(arguments []string) (jobOptions, error) {
	options := jobOptions{
		Name:        os.Getenv(model.NameEnvironment),
		ObserverURL: defaultObserver,
	}
	set := flag.NewFlagSet("sandcamp-e2e-agent job", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&options.Name, "name", options.Name, "process name")
	set.StringVar(&options.ObserverURL, "observer-url", options.ObserverURL, "observer base URL")
	set.DurationVar(&options.Delay, "delay", 0, "delay before completion")
	set.IntVar(&options.ExitCode, "exit-code", 0, "completion exit code")
	set.Var(&options.Writes, "write", "write PATH=VALUE before completion (repeatable)")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		return jobOptions{}, errors.New("invalid job arguments")
	}
	if options.Name == "" || os.Getenv(model.RunIDEnvironment) == "" || os.Getenv(model.TokenEnvironment) == "" {
		return jobOptions{}, errors.New("job name, run ID, and token are required")
	}
	if options.Delay < 0 || options.ExitCode < 0 || options.ExitCode > 125 {
		return jobOptions{}, errors.New("invalid job delay or exit code")
	}
	return options, nil
}

func runJob(options jobOptions) int {
	runID := os.Getenv(model.RunIDEnvironment)
	token := os.Getenv(model.TokenEnvironment)
	postEvent(options.ObserverURL, token, model.Event{
		Time: time.Now().UTC(), RunID: runID, Process: options.Name, Kind: "job-started", PID: os.Getpid(),
	})
	for _, declaration := range options.Writes {
		if err := writeFixtureFile(declaration); err != nil {
			postEvent(options.ObserverURL, token, model.Event{
				Time: time.Now().UTC(), RunID: runID, Process: options.Name, Kind: "job-write-failed", PID: os.Getpid(), Details: map[string]any{"error": err.Error()},
			})
			return 1
		}
	}
	if options.Delay > 0 {
		time.Sleep(options.Delay)
	}
	postEvent(options.ObserverURL, token, model.Event{
		Time: time.Now().UTC(), RunID: runID, Process: options.Name, Kind: "job-completed", PID: os.Getpid(), Details: map[string]any{"exit_code": options.ExitCode},
	})
	return options.ExitCode
}

func parseBootstrapOptions(arguments []string) (bootstrapOptions, error) {
	options := bootstrapOptions{Listen: "0.0.0.0:18080"}
	set := flag.NewFlagSet("sandcamp-e2e-agent bootstrap-observer", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&options.RunID, "run-id", "", "fixture run ID")
	set.StringVar(&options.Token, "token", "", "observer authorization token")
	set.StringVar(&options.Listen, "listen", options.Listen, "observer listen address")
	if err := set.Parse(arguments); err != nil {
		return bootstrapOptions{}, err
	}
	options.Command = append([]string(nil), set.Args()...)
	if options.RunID == "" || options.Token == "" {
		return bootstrapOptions{}, errors.New("run ID and token are required")
	}
	if len(options.Command) == 0 || !filepath.IsAbs(options.Command[0]) {
		return bootstrapOptions{}, errors.New("an absolute command is required after --")
	}
	if _, _, err := net.SplitHostPort(options.Listen); err != nil {
		return bootstrapOptions{}, fmt.Errorf("invalid listen address: %w", err)
	}
	return options, nil
}

// runBootstrap starts the test-only root observer outside campd's managed
// process set, waits until it is listening, and then replaces PID 1 with the
// real runtime command. This lets lifecycle tests observe the entire campd
// shutdown grace period. The production runtime image does not contain this
// executable.
func runBootstrap(options bootstrapOptions) error {
	observer := exec.Command(os.Args[0], "observer", "--name", "observer", "--listen", options.Listen)
	observer.Stdout = os.Stdout
	observer.Stderr = os.Stderr
	observer.Env = replaceEnvironment(os.Environ(), map[string]string{
		model.RunIDEnvironment: options.RunID,
		model.NameEnvironment:  "observer",
		model.TokenEnvironment: options.Token,
	})
	if err := observer.Start(); err != nil {
		return fmt.Errorf("start external observer: %w", err)
	}
	cleanup := func() {
		_ = observer.Process.Kill()
		_ = observer.Wait()
	}
	if err := waitForListener(options.Listen, 5*time.Second); err != nil {
		cleanup()
		return err
	}
	if err := syscall.Exec(options.Command[0], options.Command, os.Environ()); err != nil {
		cleanup()
		return fmt.Errorf("exec runtime command: %w", err)
	}
	return nil
}

func waitForListener(listen string, timeout time.Duration) error {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	target := net.JoinHostPort("127.0.0.1", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		connection, dialErr := net.DialTimeout("tcp4", target, 100*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("observer did not listen on %s within %s", target, timeout)
}

func replaceEnvironment(input []string, replacements map[string]string) []string {
	result := make([]string, 0, len(input)+len(replacements))
	for _, declaration := range input {
		name, _, found := strings.Cut(declaration, "=")
		if found {
			if _, replaced := replacements[name]; replaced {
				continue
			}
		}
		result = append(result, declaration)
	}
	for name, value := range replacements {
		result = append(result, name+"="+value)
	}
	return result
}

func parseServeOptions(arguments []string, observer bool) serveOptions {
	options := serveOptions{Observer: observer}
	set := flag.NewFlagSet("sandcamp-e2e-agent", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&options.Name, "name", os.Getenv(model.NameEnvironment), "process name")
	set.StringVar(&options.Listen, "listen", "127.0.0.1:0", "HTTP listen address")
	set.Var(&options.ExtraListen, "extra-listen", "additional HTTP listen address (repeatable)")
	set.StringVar(&options.ProbePath, "probe-path", "/healthz", "readiness probe path")
	set.StringVar(&options.ObserverURL, "observer-url", defaultObserver, "observer base URL")
	set.DurationVar(&options.ReadyDelay, "ready-delay", 0, "delay before the probe succeeds")
	set.DurationVar(&options.UnhealthyAfter, "unhealthy-after", 0, "time after which the probe fails")
	set.DurationVar(&options.ExitAfter, "exit-after", 0, "exit automatically after this duration")
	set.IntVar(&options.ExitCode, "exit-code", 0, "automatic/control exit code")
	set.DurationVar(&options.TermDelay, "term-delay", 0, "delay before exiting on a signal")
	set.BoolVar(&options.IgnoreTerm, "ignore-term", false, "record termination signals without exiting")
	set.StringVar(&options.Spawn, "spawn", "", "spawn a child in process-group or session mode")
	set.BoolVar(&options.ChildIgnoreTerm, "child-ignore-term", false, "make the spawned child ignore termination")
	set.Var(&options.Writes, "write", "write PATH=VALUE before serving (repeatable)")
	set.Var(&options.FetchOnStart, "fetch-on-start", "fetch URL after startup (repeatable)")
	if err := set.Parse(arguments); err != nil || set.NArg() != 0 {
		fatal("invalid arguments")
	}
	if options.Name == "" {
		fatal("process name is required")
	}
	if os.Getenv(model.RunIDEnvironment) == "" || os.Getenv(model.TokenEnvironment) == "" {
		fatal("run ID and token environment variables are required")
	}
	if observer {
		options.ObserverURL = ""
	}
	return options
}

func runServer(options serveOptions) error {
	runID := os.Getenv(model.RunIDEnvironment)
	token := os.Getenv(model.TokenEnvironment)
	startedAt := time.Now().UTC()
	for _, declaration := range options.Writes {
		if err := writeFixtureFile(declaration); err != nil {
			return err
		}
	}

	listener, err := net.Listen("tcp4", options.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", options.Listen, err)
	}
	defer listener.Close()

	store := &eventStore{}
	var forcedUnhealthy atomic.Bool
	probeWasReady := false
	probeMu := sync.Mutex{}
	event := func(kind string, details map[string]any) {
		item := model.Event{
			Time:    time.Now().UTC(),
			RunID:   runID,
			Process: options.Name,
			Kind:    kind,
			PID:     os.Getpid(),
			Details: details,
		}
		if options.Observer {
			store.add(item)
			return
		}
		postEvent(options.ObserverURL, token, item)
	}

	ready := func() bool {
		elapsed := time.Since(startedAt)
		if forcedUnhealthy.Load() || elapsed < options.ReadyDelay {
			return false
		}
		return options.UnhealthyAfter == 0 || elapsed < options.UnhealthyAfter
	}

	mux := http.NewServeMux()
	probeHandler := func(response http.ResponseWriter, _ *http.Request) {
		isReady := ready()
		probeMu.Lock()
		if isReady != probeWasReady {
			probeWasReady = isReady
			event("probe-transition", map[string]any{"ready": isReady})
		}
		probeMu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("X-Sandcamp-E2E-Process", options.Name)
		if !isReady {
			response.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"name":  options.Name,
			"ready": isReady,
			"pid":   os.Getpid(),
		})
	}
	mux.HandleFunc(options.ProbePath, probeHandler)
	if options.ProbePath != "/healthz" {
		mux.HandleFunc("/healthz", probeHandler)
	}
	mux.HandleFunc("/v1/self", func(response http.ResponseWriter, request *http.Request) {
		if !authorized(request, token) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		process, scanErr := scanProcess(os.Getpid(), runID)
		writeJSON(response, process, scanErr)
	})
	mux.HandleFunc("/v1/action", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || !authorized(request, token) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch request.URL.Query().Get("action") {
		case "healthy":
			forcedUnhealthy.Store(false)
			event("health-control", map[string]any{"ready": true})
		case "unhealthy":
			forcedUnhealthy.Store(true)
			event("health-control", map[string]any{"ready": false})
		case "exit":
			event("control-exit", map[string]any{"exit_code": options.ExitCode})
			go func() {
				time.Sleep(50 * time.Millisecond)
				os.Exit(options.ExitCode)
			}()
		default:
			http.Error(response, "unknown action", http.StatusBadRequest)
			return
		}
		writeJSON(response, map[string]bool{"accepted": true}, nil)
	})
	mux.HandleFunc("/v1/fetch", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || !authorized(request, token) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		var input fetchInput
		if err := decodeRequestBody(request, &input); err != nil {
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		target, err := input.target()
		if err != nil {
			http.Error(response, "invalid target", http.StatusBadRequest)
			return
		}
		writeJSON(response, fetchURL(request.Context(), target), nil)
	})

	if options.Observer {
		installObserverHandlers(mux, store, runID, token)
	}

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	serverErrors := make(chan error, 1)
	listeners := []net.Listener{listener}
	for _, address := range options.ExtraListen {
		extra, listenErr := net.Listen("tcp4", address)
		if listenErr != nil {
			return fmt.Errorf("listen %s: %w", address, listenErr)
		}
		listeners = append(listeners, extra)
	}
	for _, current := range listeners {
		go func(current net.Listener) {
			error := server.Serve(current)
			if !errors.Is(error, http.ErrServerClosed) {
				serverErrors <- error
			}
		}(current)
	}

	event("started", map[string]any{
		"address":         listener.Addr().String(),
		"extra_addresses": append([]string(nil), options.ExtraListen...),
		"cwd":             mustGetwd(),
		"uid":             os.Getuid(),
		"gid":             os.Getgid(),
	})
	for _, value := range options.FetchOnStart {
		result := fetchURL(context.Background(), value)
		event("startup-fetch", map[string]any{"result": result})
	}
	if options.Spawn != "" {
		if err := spawnChild(options, event); err != nil {
			return err
		}
	}

	if options.ExitAfter > 0 {
		go func() {
			time.Sleep(options.ExitAfter)
			event("automatic-exit", map[string]any{"exit_code": options.ExitCode})
			time.Sleep(50 * time.Millisecond)
			os.Exit(options.ExitCode)
		}()
	}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	for {
		select {
		case received := <-signals:
			event("signal", map[string]any{"signal": received.String()})
			if options.IgnoreTerm {
				continue
			}
			if options.TermDelay > 0 {
				time.Sleep(options.TermDelay)
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = server.Shutdown(shutdownCtx)
			cancel()
			return nil
		case serverErr := <-serverErrors:
			return serverErr
		}
	}
}

func installObserverHandlers(mux *http.ServeMux, store *eventStore, runID, token string) {
	mux.HandleFunc("/v1/events", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || !authorized(request, token) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		defer request.Body.Close()
		decoder := json.NewDecoder(io.LimitReader(request.Body, maxBodyBytes))
		var event model.Event
		if err := decoder.Decode(&event); err != nil || event.RunID != runID {
			http.Error(response, "invalid event", http.StatusBadRequest)
			return
		}
		store.add(event)
		response.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1/snapshot", func(response http.ResponseWriter, request *http.Request) {
		if !authorized(request, token) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		processes, err := scanRun(runID)
		writeJSON(response, model.Snapshot{
			ObservedAt: time.Now().UTC(),
			RunID:      runID,
			Events:     store.copy(),
			Processes:  processes,
		}, err)
	})
	mux.HandleFunc("/v1/file", func(response http.ResponseWriter, request *http.Request) {
		if !authorized(request, token) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(response, readProcessFile(
			runID,
			request.URL.Query().Get("process"),
			request.URL.Query().Get("path"),
		), nil)
	})
	mux.HandleFunc("/v1/relay", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || !authorized(request, token) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		var input struct {
			Port   int    `json:"port"`
			Action string `json:"action"`
		}
		if err := decodeRequestBody(request, &input); err != nil {
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		if input.Port < 1 || input.Port > 65535 ||
			input.Action != "exit" && input.Action != "healthy" && input.Action != "unhealthy" {
			http.Error(response, "invalid action target", http.StatusBadRequest)
			return
		}
		target := fmt.Sprintf("http://127.0.0.1:%d/v1/action?action=%s", input.Port, input.Action)
		requestCtx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
		defer cancel()
		relay, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target, nil)
		if err != nil {
			writeJSON(response, nil, err)
			return
		}
		relay.Header.Set(observerHeader, token)
		relayResponse, err := http.DefaultClient.Do(relay)
		if err != nil {
			writeJSON(response, nil, err)
			return
		}
		defer relayResponse.Body.Close()
		body, err := io.ReadAll(io.LimitReader(relayResponse.Body, maxBodyBytes))
		writeJSON(response, map[string]any{
			"status_code": relayResponse.StatusCode,
			"body":        string(body),
		}, err)
	})
}

type fetchInput struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port,omitempty"`
	Path   string `json:"path"`
}

func (input fetchInput) target() (string, error) {
	if input.Scheme != "http" && input.Scheme != "https" {
		return "", errors.New("unsupported scheme")
	}
	host := input.Host
	if host == "loopback" {
		host = "127.0.0.1"
	} else if host == "" || strings.ContainsAny(host, "/:@[] \t\r\n") {
		return "", errors.New("invalid host")
	}
	if input.Port < 0 || input.Port > 65535 {
		return "", errors.New("invalid port")
	}
	if input.Path == "" {
		input.Path = "/"
	}
	if !strings.HasPrefix(input.Path, "/") || strings.ContainsAny(input.Path, "\r\n") {
		return "", errors.New("invalid path")
	}
	if input.Port != 0 {
		host = net.JoinHostPort(host, strconv.Itoa(input.Port))
	}
	return input.Scheme + "://" + host + input.Path, nil
}

func decodeRequestBody(request *http.Request, target any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("request contains extra JSON")
	}
	return nil
}

func authorized(request *http.Request, token string) bool {
	return token != "" && request.Header.Get(observerHeader) == token
}

func postEvent(observerURL, token string, event model.Event) {
	body, err := json.Marshal(event)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(observerURL, "/")+"/v1/events", bytes.NewReader(body))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(observerHeader, token)
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		_ = response.Body.Close()
	}
}

func scanRun(runID string) ([]model.Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	processes := make([]model.Process, 0)
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || !entry.IsDir() {
			continue
		}
		environment, readErr := readProcessEnvironment(pid)
		if readErr != nil || environment[model.RunIDEnvironment] != runID {
			continue
		}
		process, scanErr := scanProcess(pid, runID)
		if scanErr == nil {
			processes = append(processes, process)
		}
	}
	sort.Slice(processes, func(left, right int) bool {
		if processes[left].Name == processes[right].Name {
			return processes[left].PID < processes[right].PID
		}
		return processes[left].Name < processes[right].Name
	})
	return processes, nil
}

func scanProcess(pid int, runID string) (model.Process, error) {
	environment, err := readProcessEnvironment(pid)
	if err != nil {
		return model.Process{}, err
	}
	if runID != "" && environment[model.RunIDEnvironment] != runID {
		return model.Process{}, errors.New("process belongs to another run")
	}
	status, err := readStatus(pid)
	if err != nil {
		return model.Process{}, err
	}
	result := model.Process{
		Name:        environment[model.NameEnvironment],
		PID:         pid,
		PPID:        firstInteger(status["PPid"]),
		UID:         firstInteger(status["Uid"]),
		GID:         firstInteger(status["Gid"]),
		Groups:      integers(status["Groups"]),
		Status:      selectedStatus(status),
		Environment: selectedEnvironment(environment),
		Cwd:         readLink(filepath.Join("/proc", strconv.Itoa(pid), "cwd")),
		Executable:  readLink(filepath.Join("/proc", strconv.Itoa(pid), "exe")),
		Command:     readNullSeparated(filepath.Join("/proc", strconv.Itoa(pid), "cmdline")),
		Namespaces:  readNamespaces(pid),
		RootFSType:  rootFilesystemType(pid),
	}
	if declared := environment[model.FilesEnvironment]; declared != "" {
		result.Files = make(map[string]model.FileResult)
		for _, path := range strings.Split(declared, ",") {
			path = strings.TrimSpace(path)
			if path != "" {
				result.Files[path] = readPIDFile(pid, result.Name, path)
			}
		}
	}
	if result.Name == "" {
		result.Name = status["Name"]
	}
	return result, nil
}

func readProcessEnvironment(pid int) (map[string]string, error) {
	contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, item := range bytes.Split(contents, []byte{0}) {
		key, value, found := bytes.Cut(item, []byte{'='})
		if found {
			result[string(key)] = string(value)
		}
	}
	return result, nil
}

func selectedEnvironment(environment map[string]string) map[string]string {
	result := make(map[string]string)
	for key, value := range environment {
		if key == model.TokenEnvironment {
			continue
		}
		if strings.HasPrefix(key, "SANDCAMP_E2E_") || key == "HOME" || key == "USER" || key == "LOGNAME" || key == "PATH" {
			result[key] = value
		}
	}
	return result
}

func readStatus(pid int) (map[string]string, error) {
	contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, line := range strings.Split(string(contents), "\n") {
		key, value, found := strings.Cut(line, ":")
		if found {
			result[key] = strings.TrimSpace(value)
		}
	}
	return result, nil
}

func selectedStatus(status map[string]string) map[string]string {
	keys := []string{"Name", "State", "PPid", "Uid", "Gid", "Groups", "NSpid", "NSpgid", "NSsid", "CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb", "NoNewPrivs", "Seccomp"}
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, found := status[key]; found {
			result[key] = value
		}
	}
	return result
}

func readNamespaces(pid int) map[string]string {
	result := make(map[string]string)
	for _, namespace := range []string{"cgroup", "ipc", "mnt", "net", "pid", "pid_for_children", "time", "user", "uts"} {
		value := readLink(filepath.Join("/proc", strconv.Itoa(pid), "ns", namespace))
		if value != "" {
			result[namespace] = value
		}
	}
	return result
}

func rootFilesystemType(pid int) string {
	contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "mountinfo"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(contents), "\n") {
		left, right, found := strings.Cut(line, " - ")
		if !found {
			continue
		}
		fields := strings.Fields(left)
		rightFields := strings.Fields(right)
		if len(fields) > 4 && fields[4] == "/" && len(rightFields) > 0 {
			return rightFields[0]
		}
	}
	return ""
}

func readProcessFile(runID, processName, path string) model.FileResult {
	result := model.FileResult{Process: processName, Path: path}
	if processName == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		result.Error = "process and clean absolute path are required"
		return result
	}
	processes, err := scanRun(runID)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	pid := 0
	for _, process := range processes {
		if process.Name == processName {
			pid = process.PID
			break
		}
	}
	if pid == 0 {
		result.Error = "process not found"
		return result
	}
	return readPIDFile(pid, processName, path)
}

func readPIDFile(pid int, processName, path string) model.FileResult {
	result := model.FileResult{Process: processName, Path: path}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		result.Error = "clean absolute path is required"
		return result
	}
	fullPath := filepath.Join("/proc", strconv.Itoa(pid), "root", strings.TrimPrefix(path, "/"))
	info, err := os.Stat(fullPath)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Mode = info.Mode().String()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		result.UID = int(stat.Uid)
		result.GID = int(stat.Gid)
	}
	file, err := os.Open(fullPath)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if len(contents) > maxFileBytes {
		result.Error = "file exceeds evidence limit"
		return result
	}
	result.Content = string(contents)
	return result
}

func fetchURL(ctx context.Context, value string) model.FetchResult {
	result := model.FetchResult{URL: value}
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		result.Error = "only HTTP and HTTPS URLs are supported"
		return result
	}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, value, nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer response.Body.Close()
	result.StatusCode = response.StatusCode
	result.Header = map[string]string{
		"Content-Type":           response.Header.Get("Content-Type"),
		"X-Sandcamp-E2E-Process": response.Header.Get("X-Sandcamp-E2E-Process"),
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if len(body) > maxBodyBytes {
		result.Error = "response exceeds evidence limit"
		return result
	}
	result.Body = string(body)
	return result
}

func spawnChild(options serveOptions, event func(string, map[string]any)) error {
	if options.Spawn != "process-group" && options.Spawn != "session" {
		return fmt.Errorf("unsupported spawn mode %q", options.Spawn)
	}
	childName := options.Name + "-child"
	command := exec.Command(os.Args[0], "child", "--name", childName, "--observer-url", options.ObserverURL)
	if options.ChildIgnoreTerm {
		command.Args = append(command.Args, "--ignore-term")
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = append(os.Environ(), model.NameEnvironment+"="+childName)
	if options.Spawn == "session" {
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("spawn child: %w", err)
	}
	event("child-spawned", map[string]any{"mode": options.Spawn, "pid": command.Process.Pid, "name": childName})
	go func() { _ = command.Wait() }()
	return nil
}

func runChild(arguments []string) error {
	options := parseServeOptions(arguments, false)
	options.Listen = "127.0.0.1:0"
	return runServer(options)
}

func writeFixtureFile(declaration string) error {
	path, value, found := strings.Cut(declaration, "=")
	if !found || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("invalid --write declaration %q", declaration)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent for %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func readNullSeparated(path string) []string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	items := bytes.Split(bytes.TrimRight(contents, "\x00"), []byte{0})
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, string(item))
	}
	return result
}

func readLink(path string) string {
	value, _ := os.Readlink(path)
	return value
}

func firstInteger(value string) int {
	values := integers(value)
	if len(values) == 0 {
		return -1
	}
	return values[0]
}

func integers(value string) []int {
	result := make([]int, 0)
	for _, field := range strings.Fields(value) {
		parsed, err := strconv.Atoi(field)
		if err == nil {
			result = append(result, parsed)
		}
	}
	return result
}

func mustGetwd() string {
	value, _ := os.Getwd()
	return value
}

func writeJSON(response http.ResponseWriter, value any, err error) {
	response.Header().Set("Content-Type", "application/json")
	if err != nil {
		response.WriteHeader(http.StatusInternalServerError)
		value = map[string]string{"error": err.Error()}
	}
	encoder := json.NewEncoder(response)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "sandcamp-e2e-agent:", message)
	os.Exit(2)
}
