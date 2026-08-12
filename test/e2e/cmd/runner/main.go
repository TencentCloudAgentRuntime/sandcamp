package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TencentCloudAgentRuntime/sandcamp"
	"github.com/TencentCloudAgentRuntime/sandcamp/test/e2e/internal/model"
	"github.com/TencentCloudAgentRuntime/sandcamp/test/e2e/internal/scenario"
	ags "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/ags/v20250920"
)

const (
	pass                = "PASS"
	fail                = "FAIL"
	expectedReject      = "EXPECTED_REJECT"
	defaultRegion       = "ap-guangzhou"
	toolTimeout         = 10 * time.Minute
	instanceTimeout     = 10 * time.Minute
	commandTimeout      = 60 * time.Second
	proxyTimeout        = 45 * time.Second
	pollInterval        = 3 * time.Second
	maxCommandOutput    = 1 << 20
	observerTokenHeader = "X-Sandcamp-E2E-Token"
)

type options struct {
	Command       string
	Scenario      string
	OutputDir     string
	KeepOnFailure bool
	AGR           string
}

type environment struct {
	Region  string
	RoleARN string
	Images  scenario.Images
}

type check struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Summary  string `json:"summary"`
	Evidence any    `json:"evidence,omitempty"`
}

type scenarioResult struct {
	Name             string                  `json:"name"`
	Category         string                  `json:"category"`
	RunID            string                  `json:"run_id"`
	InstanceID       string                  `json:"instance_id,omitempty"`
	StartedAt        time.Time               `json:"started_at"`
	EndedAt          time.Time               `json:"ended_at"`
	Status           string                  `json:"status"`
	Checks           []check                 `json:"checks"`
	Snapshot         *model.Snapshot         `json:"snapshot,omitempty"`
	LifecycleActions []lifecycleActionResult `json:"lifecycle_actions,omitempty"`
	Restarted        *model.Snapshot         `json:"restarted_snapshot,omitempty"`
	Error            string                  `json:"error,omitempty"`
}

type lifecycleActionResult struct {
	Name         string                       `json:"name"`
	Snapshot     model.Snapshot               `json:"snapshot"`
	Observations map[string]model.FetchResult `json:"observations,omitempty"`
}

type report struct {
	SchemaVersion string            `json:"schema_version"`
	RunID         string            `json:"run_id"`
	Region        string            `json:"region"`
	ToolID        string            `json:"tool_id,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	EndedAt       time.Time         `json:"ended_at"`
	Scenarios     []scenarioResult  `json:"scenarios"`
	Cleanup       map[string]string `json:"cleanup"`
	Conclusion    string            `json:"conclusion"`
}

type runner struct {
	ctx    context.Context
	opts   options
	env    environment
	report report
	toolID string
	failed bool
}

type toolCreateRequest struct {
	ToolName             string                    `json:"ToolName"`
	ToolType             string                    `json:"ToolType"`
	Description          string                    `json:"Description"`
	DefaultTimeout       string                    `json:"DefaultTimeout"`
	RoleArn              string                    `json:"RoleArn"`
	ClientToken          string                    `json:"ClientToken"`
	NetworkConfiguration *ags.NetworkConfiguration `json:"NetworkConfiguration"`
	StorageMounts        []*ags.StorageMount       `json:"StorageMounts"`
	CustomConfiguration  *ags.CustomConfiguration  `json:"CustomConfiguration"`
}

type instanceCreateRequest struct {
	ToolID              string                   `json:"ToolId"`
	Timeout             string                   `json:"Timeout"`
	ClientToken         string                   `json:"ClientToken"`
	AuthMode            string                   `json:"AuthMode"`
	CustomConfiguration *ags.CustomConfiguration `json:"CustomConfiguration"`
}

type agrEnvelope struct {
	SchemaVersion string          `json:"SchemaVersion"`
	Status        string          `json:"Status"`
	Data          json.RawMessage `json:"Data"`
	Failure       any             `json:"Failure"`
}

type agrResult struct {
	Envelope agrEnvelope
	ExitCode int
	Stderr   string
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		usage(os.Stdout)
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		usage(os.Stderr)
		os.Exit(2)
	}
	if opts.Command == "list" {
		listScenarios()
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env, err := loadEnvironment()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if opts.Command == "doctor" {
		if err := doctor(ctx, opts, env); err != nil {
			fmt.Fprintln(os.Stderr, "FAIL", redact(err.Error()))
			os.Exit(1)
		}
		fmt.Printf("PASS credentials and read-only AGS access are available in %s\n", env.Region)
		return
	}

	run := &runner{
		ctx:  ctx,
		opts: opts,
		env:  env,
		report: report{
			SchemaVersion: "sandcamp.e2e.v2",
			RunID:         newID("run"),
			Region:        env.Region,
			StartedAt:     time.Now().UTC(),
			Cleanup:       make(map[string]string),
		},
	}
	if err := run.execute(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL", redact(err.Error()))
		os.Exit(1)
	}
}

func parseOptions(arguments []string) (options, error) {
	if len(arguments) == 0 {
		return options{}, errors.New("a command is required")
	}
	opts := options{Command: arguments[0], Scenario: "minimal-root", OutputDir: "test/e2e/evidence", AGR: "agr"}
	if opts.Command != "doctor" && opts.Command != "list" && opts.Command != "run" {
		return options{}, fmt.Errorf("unknown command %q", opts.Command)
	}
	set := flag.NewFlagSet("sandcamp-e2e "+opts.Command, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&opts.Scenario, "scenario", opts.Scenario, "scenario name or all")
	set.StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "evidence directory")
	set.BoolVar(&opts.KeepOnFailure, "keep-on-failure", false, "keep cloud resources after a failure")
	set.StringVar(&opts.AGR, "agr", opts.AGR, "agr executable")
	if err := set.Parse(arguments[1:]); err != nil {
		return options{}, err
	}
	if set.NArg() != 0 {
		return options{}, errors.New("unexpected positional arguments")
	}
	if opts.Command == "run" {
		if _, err := selectScenarios(opts.Scenario); err != nil {
			return options{}, err
		}
	}
	return opts, nil
}

func usage(writer io.Writer) {
	fmt.Fprintln(writer, "usage:")
	fmt.Fprintln(writer, "  go run ./test/e2e/cmd/runner list")
	fmt.Fprintln(writer, "  go run ./test/e2e/cmd/runner doctor [--agr=agr]")
	fmt.Fprintln(writer, "  go run ./test/e2e/cmd/runner run [--scenario=name[,name...]|all] [--output-dir=dir] [--keep-on-failure]")
}

func listScenarios() {
	for _, item := range scenario.Core() {
		fmt.Printf("%-24s %-14s %s\n", item.Name, item.Category, item.Description)
	}
}

func loadEnvironment() (environment, error) {
	get := func(name string) string { return strings.TrimSpace(os.Getenv(name)) }
	missing := make([]string, 0)
	for _, name := range []string{"TENCENTCLOUD_SECRET_ID", "TENCENTCLOUD_SECRET_KEY", "AGS_ROLE_ARN", "SANDCAMP_E2E_RUNTIME_IMAGE", "SANDCAMP_E2E_AGENT_IMAGE", "SANDCAMP_E2E_MAIN_IMAGE"} {
		if get(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		return environment{}, fmt.Errorf("missing environment variables: %s", strings.Join(missing, ", "))
	}
	registryType := get("SANDCAMP_E2E_REGISTRY_TYPE")
	if registryType == "" {
		registryType = "personal"
	}
	agentGlibc := get("SANDCAMP_E2E_GLIBC_IMAGE")
	if agentGlibc == "" {
		agentGlibc = get("SANDCAMP_E2E_AGENT_IMAGE")
	}
	region := get("TENCENTCLOUD_REGION")
	if region == "" {
		region = defaultRegion
	}
	return environment{
		Region:  region,
		RoleARN: get("AGS_ROLE_ARN"),
		Images: scenario.Images{
			Runtime:      get("SANDCAMP_E2E_RUNTIME_IMAGE"),
			AgentAlpine:  get("SANDCAMP_E2E_AGENT_IMAGE"),
			AgentGlibc:   agentGlibc,
			FastAPI:      get("SANDCAMP_E2E_FASTAPI_IMAGE"),
			Egress:       get("SANDCAMP_E2E_EGRESS_IMAGE"),
			Nginx:        get("SANDCAMP_E2E_NGINX_IMAGE"),
			Envd:         get("SANDCAMP_E2E_ENVD_IMAGE"),
			Main:         get("SANDCAMP_E2E_MAIN_IMAGE"),
			RegistryType: sandcamp.ImageRegistryType(registryType),
		},
	}, nil
}

func doctor(ctx context.Context, opts options, env environment) error {
	if _, err := exec.LookPath(opts.AGR); err != nil {
		return fmt.Errorf("find agr: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	result, err := runAGR(callCtx, opts.AGR, nil,
		"--region", env.Region, "--non-interactive", "--no-color", "tool", "list", "--limit", "1", "-o", "json")
	if err != nil || !agrSucceeded(result) {
		return fmt.Errorf("read-only AGS query failed: %s", agrFailure(result, err))
	}
	return nil
}

func (r *runner) execute() (returnedErr error) {
	if err := os.MkdirAll(r.opts.OutputDir, 0o755); err != nil {
		return err
	}
	defer func() {
		cleanupErr := r.cleanup()
		if cleanupErr != nil && returnedErr == nil {
			returnedErr = cleanupErr
		}
		r.report.EndedAt = time.Now().UTC()
		if returnedErr != nil || r.failed || cleanupErr != nil {
			r.report.Conclusion = fail
		} else {
			r.report.Conclusion = pass
		}
		path, writeErr := r.writeEvidence()
		if writeErr != nil {
			fmt.Fprintln(os.Stderr, "FAIL write evidence:", writeErr)
			if returnedErr == nil {
				returnedErr = writeErr
			}
		} else {
			fmt.Println("evidence:", path)
		}
		fmt.Println("conclusion:", r.report.Conclusion)
	}()

	if err := doctor(r.ctx, r.opts, r.env); err != nil {
		return err
	}
	items, err := selectScenarios(r.opts.Scenario)
	if err != nil {
		return err
	}
	if missing := missingImages(items, r.env.Images); len(missing) != 0 {
		return fmt.Errorf("selected scenarios require missing images: %s", strings.Join(missing, ", "))
	}
	if err := r.createTool(items[0]); err != nil {
		return err
	}
	for _, item := range items {
		result := r.runScenario(item)
		r.report.Scenarios = append(r.report.Scenarios, result)
		fmt.Printf("%s scenario=%s checks=%d instance=%s\n", result.Status, result.Name, len(result.Checks), result.InstanceID)
		if result.Status == fail {
			r.failed = true
		}
	}
	if r.failed {
		return errors.New("one or more scenarios failed")
	}
	return nil
}

func selectScenarios(value string) ([]scenario.Scenario, error) {
	if value == "all" {
		return scenario.Core(), nil
	}
	selected := make([]scenario.Scenario, 0)
	seen := make(map[string]struct{})
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, errors.New("scenario selection contains an empty name")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("scenario %q is selected more than once", name)
		}
		item, found := scenario.ByName(name)
		if !found {
			return nil, fmt.Errorf("unknown scenario %q", name)
		}
		seen[name] = struct{}{}
		selected = append(selected, item)
	}
	return selected, nil
}

func (r *runner) createTool(defaultScenario scenario.Scenario) error {
	images := scenario.CoreImageSet(r.env.Images)
	mounts, err := sandcamp.RenderMounts(images)
	if err != nil {
		return fmt.Errorf("render tool mounts: %w", err)
	}
	if r.env.Images.Envd != "" {
		mounts = append(mounts, &ags.StorageMount{
			Name:      stringPointer("envd-runtime"),
			MountPath: stringPointer("/mnt/envd-runtime/envd"),
			ReadOnly:  boolPointer(true),
			StorageSource: &ags.StorageSource{Image: &ags.ImageStorageSource{
				Reference:         stringPointer(r.env.Images.Envd),
				ImageRegistryType: stringPointer(string(r.env.Images.RegistryType)),
				SubPath:           stringPointer("/usr/bin/envd"),
			}},
		})
	}
	configuration, err := sandcamp.RenderStart(images, defaultScenario.Build(newID("default"), randomToken()))
	if err != nil {
		return fmt.Errorf("render tool start: %w", err)
	}
	configuration.Image = stringPointer(mainImage(defaultScenario, r.env.Images))
	configuration.ImageRegistryType = stringPointer(string(r.env.Images.RegistryType))
	configuration.Resources = &ags.ResourceConfiguration{CPU: stringPointer("2"), Memory: stringPointer("4Gi")}
	request := toolCreateRequest{
		ToolName:             "sandcamp-e2e-" + strings.TrimPrefix(r.report.RunID, "run-"),
		ToolType:             "custom",
		Description:          "Sandcamp runtime regression suite",
		DefaultTimeout:       "15m",
		RoleArn:              r.env.RoleARN,
		ClientToken:          newID("tool"),
		NetworkConfiguration: &ags.NetworkConfiguration{NetworkMode: stringPointer("PUBLIC")},
		StorageMounts:        mounts,
		CustomConfiguration:  configuration,
	}
	response, err := r.agrJSON(r.ctx, request, "tool", "create", "--request", "-", "-o", "json")
	if err != nil {
		return fmt.Errorf("create tool: %w", err)
	}
	var created struct {
		ToolID string `json:"ToolId"`
	}
	if err := decodeData(response, &created); err != nil || created.ToolID == "" {
		return fmt.Errorf("decode tool creation: %w", err)
	}
	r.toolID = created.ToolID
	r.report.ToolID = created.ToolID
	fmt.Println("tool:", created.ToolID)
	return r.waitForState("tool", created.ToolID, toolTimeout, "ACTIVE", "FAILED", "DELETING")
}

func (r *runner) runScenario(item scenario.Scenario) (result scenarioResult) {
	result = scenarioResult{
		Name:      item.Name,
		Category:  item.Category,
		RunID:     newID(item.Name),
		StartedAt: time.Now().UTC(),
		Status:    fail,
	}
	defer func() { result.EndedAt = time.Now().UTC() }()
	token := randomToken()
	configuration, err := sandcamp.RenderStart(scenario.CoreImageSet(r.env.Images), item.Build(result.RunID, token))
	if err != nil {
		result.Error = redact(err.Error())
		return result
	}
	if item.Configure != nil {
		item.Configure(configuration, result.RunID, token)
	}
	configuration.Image = stringPointer(mainImage(item, r.env.Images))
	configuration.ImageRegistryType = stringPointer(string(r.env.Images.RegistryType))
	response, err := r.agrJSON(r.ctx, instanceCreateRequest{
		ToolID:              r.toolID,
		Timeout:             "15m",
		ClientToken:         newID("instance"),
		AuthMode:            "TOKEN",
		CustomConfiguration: configuration,
	}, "instance", "create", "--request", "-", "-o", "json")
	if err != nil {
		code := failureCode(response)
		if item.ExpectedState == scenario.ExpectedStopped && expectedRejectionCode(code) {
			result.Status = expectedReject
			result.Checks = append(result.Checks, check{
				Name:     "expected-start-rejection",
				Status:   expectedReject,
				Summary:  "the intentionally invalid initialization failed before the instance reached RUNNING",
				Evidence: map[string]string{"code": code},
			})
			return result
		}
		result.Error = redact(err.Error())
		return result
	}
	var created struct {
		InstanceID string `json:"InstanceId"`
		Instance   struct {
			InstanceID string `json:"InstanceId"`
		} `json:"Instance"`
	}
	if err := decodeData(response, &created); err != nil {
		result.Error = err.Error()
		return result
	}
	if created.InstanceID == "" {
		created.InstanceID = created.Instance.InstanceID
	}
	result.InstanceID = created.InstanceID
	if result.InstanceID == "" {
		result.Error = "instance creation did not return an ID"
		return result
	}
	defer func() {
		if r.opts.KeepOnFailure && result.Status == fail {
			r.report.Cleanup["instance:"+result.InstanceID] = "retained"
			return
		}
		if err := r.deleteResource("instance", result.InstanceID); err != nil {
			r.report.Cleanup["instance:"+result.InstanceID] = "delete failed: " + redact(err.Error())
			r.failed = true
		} else {
			r.report.Cleanup["instance:"+result.InstanceID] = "deleted"
		}
	}()

	if err := r.waitForState("instance", result.InstanceID, instanceTimeout, string(item.ExpectedState), "FAILED", "STOPPED", "STOP_FAILED"); err != nil {
		result.Error = redact(err.Error())
		return result
	}
	if item.ExpectedState != scenario.ExpectedRunning {
		result.Checks = append(result.Checks, check{
			Name:     "expected-terminal-state",
			Status:   expectedReject,
			Summary:  "the intentionally invalid runtime case was rejected without reaching RUNNING",
			Evidence: map[string]any{"state": item.ExpectedState},
		})
		result.Status = expectedReject
		return result
	}
	proxy, err := r.startProxy(result.InstanceID, scenario.ObserverPort)
	if err != nil {
		result.Error = redact(err.Error())
		return result
	}
	defer proxy.stop()
	if item.Settle > 0 {
		timer := time.NewTimer(item.Settle)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			result.Error = r.ctx.Err().Error()
			return result
		case <-timer.C:
		}
	}
	observations := make(map[string]model.FetchResult, len(item.Fetches))
	valid := true
	for _, configured := range item.Fetches {
		observed, fetchErr := fetchThroughObserver(r.ctx, proxy.port, token, configured.URL)
		observations[configured.Name] = observed
		passed := fetchErr == nil && observed.StatusCode == configured.ExpectedStatus
		if configured.ExpectedProcess != "" {
			passed = passed && observed.Header["X-Sandcamp-E2E-Process"] == configured.ExpectedProcess
		}
		if !passed {
			valid = false
		}
		status := pass
		if !passed {
			status = fail
		}
		result.Checks = append(result.Checks, check{
			Name:    "active-fetch-" + configured.Name,
			Status:  status,
			Summary: "observer active fetch matches the expected status and process identity",
			Evidence: map[string]any{
				"expected_status":  configured.ExpectedStatus,
				"expected_process": configured.ExpectedProcess,
				"observed":         observed,
				"error":            errorString(fetchErr),
			},
		})
	}
	snapshot, err := fetchSnapshot(r.ctx, proxy.port, token)
	if err != nil {
		result.Error = redact(err.Error())
		return result
	}
	result.Snapshot = &snapshot
	for _, assertion := range item.Validate(snapshot, observations) {
		status := pass
		if !assertion.Passed {
			status = fail
			valid = false
		}
		result.Checks = append(result.Checks, check{
			Name: assertion.Name, Status: status, Summary: assertion.Summary, Evidence: assertion.Evidence,
		})
	}
	if item.Lifecycle != nil {
		lifecycleStarted := time.Now()
		previous := snapshot
		actionsCompleted := true
		for _, action := range item.Lifecycle.Actions {
			var triggerErr error
			if action.SignalProcess != "" {
				triggerErr = signalProcess(r.ctx, proxy.port, token, action.SignalProcess, action.Signal)
			} else {
				triggerErr = relayAction(r.ctx, proxy.port, token, action.TargetURL)
			}
			if triggerErr != nil {
				valid = false
				actionsCompleted = false
				result.Checks = append(result.Checks, check{
					Name: action.Name + "-trigger", Status: fail, Summary: "observer could not trigger the lifecycle action", Evidence: redact(triggerErr.Error()),
				})
				break
			}
			result.Checks = append(result.Checks, check{
				Name: action.Name + "-trigger", Status: pass, Summary: "observer triggered the lifecycle action",
			})
			after := captureAfterAction(r.ctx, proxy.port, token, previous, action.CaptureFor)
			observed := make(map[string]model.FetchResult, len(action.Fetches))
			for _, configured := range action.Fetches {
				fetch, fetchErr := fetchThroughObserver(r.ctx, proxy.port, token, configured.URL)
				observed[configured.Name] = fetch
				passed := fetchErr == nil && fetch.StatusCode == configured.ExpectedStatus
				if configured.ExpectedProcess != "" {
					passed = passed && fetch.Header["X-Sandcamp-E2E-Process"] == configured.ExpectedProcess
				}
				status := pass
				if !passed {
					status = fail
					valid = false
				}
				result.Checks = append(result.Checks, check{
					Name: action.Name + "-fetch-" + configured.Name, Status: status,
					Summary:  "post-action fetch matches the expected status and process identity",
					Evidence: map[string]any{"expected_status": configured.ExpectedStatus, "expected_process": configured.ExpectedProcess, "observed": fetch, "error": errorString(fetchErr)},
				})
			}
			result.LifecycleActions = append(result.LifecycleActions, lifecycleActionResult{
				Name: action.Name, Snapshot: after, Observations: observed,
			})
			if action.Validate != nil {
				for _, assertion := range action.Validate(after, observed) {
					status := pass
					if !assertion.Passed {
						status = fail
						valid = false
					}
					result.Checks = append(result.Checks, check{
						Name: action.Name + "-" + assertion.Name, Status: status, Summary: assertion.Summary, Evidence: assertion.Evidence,
					})
				}
			}
			previous = after
		}

		if actionsCompleted {
			var stateErr error
			if item.Lifecycle.FinalState == scenario.ExpectedRestarted {
				proxy.stop()
				var restarted model.Snapshot
				restarted, stateErr = r.waitForRestart(result.InstanceID, token, lifecycleStarted, snapshotProcessNames(snapshot), 90*time.Second)
				if stateErr == nil {
					result.Restarted = &restarted
				}
			} else {
				stateErr = r.waitForState("instance", result.InstanceID, instanceTimeout, string(item.Lifecycle.FinalState), "FAILED", "STOPPED", "STOP_FAILED")
			}
			stateStatus := pass
			if stateErr != nil {
				stateStatus = fail
				valid = false
			}
			result.Checks = append(result.Checks, check{
				Name: "lifecycle-final-state", Status: stateStatus,
				Summary:  "runtime reaches the expected post-action state",
				Evidence: map[string]any{"expected_state": item.Lifecycle.FinalState, "elapsed": time.Since(lifecycleStarted).String(), "error": errorString(stateErr)},
			})
		}
	}
	if valid {
		result.Status = pass
	}
	return result
}

func expectedRejectionCode(code string) bool {
	switch code {
	case "FailedOperation.ContainerStart", "FailedOperation.ContainerProbe", "FailedOperation.Timeout":
		return true
	default:
		return false
	}
}

func relayAction(ctx context.Context, localPort int, token, target string) error {
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	parsed, err := url.Parse(target)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return fmt.Errorf("invalid lifecycle target port: %w", err)
	}
	body, err := json.Marshal(map[string]any{
		"port":   port,
		"action": parsed.Query().Get("action"),
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/relay", localPort), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set(observerTokenHeader, token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("observer relay returned HTTP %d: %s", response.StatusCode, string(body))
	}
	var relayed struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&relayed); err != nil {
		return fmt.Errorf("decode observer relay: %w", err)
	}
	if relayed.StatusCode < 200 || relayed.StatusCode >= 300 {
		return fmt.Errorf("target action returned HTTP %d: %s", relayed.StatusCode, relayed.Body)
	}
	return nil
}

func signalProcess(ctx context.Context, localPort int, token, processName, signal string) error {
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string]string{"process": processName, "signal": signal})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/signal", localPort), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set(observerTokenHeader, token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("observer signal returned HTTP %d: %s", response.StatusCode, string(body))
	}
	return nil
}

func (r *runner) waitForRestart(instanceID, token string, after time.Time, expectedProcesses []string, timeout time.Duration) (model.Snapshot, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		proxy, err := r.startProxy(instanceID, scenario.ObserverPort)
		if err == nil {
			snapshot, snapshotErr := fetchSnapshot(r.ctx, proxy.port, token)
			proxy.stop()
			if snapshotErr == nil {
				started := observerStartedAt(snapshot)
				actualProcesses := snapshotProcessNames(snapshot)
				if started.After(after) && equalStrings(actualProcesses, expectedProcesses) {
					return snapshot, nil
				}
				lastErr = fmt.Errorf("new generation not complete; started=%s action=%s processes=%v expected=%v", started.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano), actualProcesses, expectedProcesses)
			} else {
				lastErr = snapshotErr
			}
		} else {
			lastErr = err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return model.Snapshot{}, r.ctx.Err()
		case <-timer.C:
		}
	}
	return model.Snapshot{}, fmt.Errorf("observer did not return in a new runtime generation: %w", lastErr)
}

func observerStartedAt(snapshot model.Snapshot) time.Time {
	for _, event := range snapshot.Events {
		if event.Process == "observer" && event.Kind == "started" {
			return event.Time
		}
	}
	return time.Time{}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func snapshotProcessNames(snapshot model.Snapshot) []string {
	result := make([]string, 0, len(snapshot.Processes))
	for _, process := range snapshot.Processes {
		result = append(result, process.Name)
	}
	sort.Strings(result)
	return result
}

func captureAfterAction(ctx context.Context, localPort int, token string, initial model.Snapshot, duration time.Duration) model.Snapshot {
	if duration <= 0 {
		duration = 2 * time.Second
	}
	latest := initial
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		callCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		snapshot, err := fetchSnapshotOnce(callCtx, localPort, token)
		cancel()
		if err == nil {
			latest = snapshot
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return latest
		case <-timer.C:
		}
	}
	return latest
}

func fetchThroughObserver(ctx context.Context, localPort int, token, target string) (model.FetchResult, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	parsed, err := url.Parse(target)
	if err != nil {
		return model.FetchResult{}, err
	}
	host := parsed.Hostname()
	if host == "127.0.0.1" || host == "localhost" {
		host = "loopback"
	}
	port := 0
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil {
			return model.FetchResult{}, err
		}
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	body, err := json.Marshal(map[string]any{
		"scheme": parsed.Scheme,
		"host":   host,
		"port":   port,
		"path":   path,
	})
	if err != nil {
		return model.FetchResult{}, err
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/fetch", localPort), bytes.NewReader(body))
	if err != nil {
		return model.FetchResult{}, err
	}
	request.Header.Set(observerTokenHeader, token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return model.FetchResult{}, err
	}
	defer response.Body.Close()
	var result model.FetchResult
	if err := json.NewDecoder(io.LimitReader(response.Body, maxCommandOutput)).Decode(&result); err != nil {
		return result, err
	}
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("observer returned HTTP %d", response.StatusCode)
	}
	return result, nil
}

func (r *runner) waitForState(kind, id string, timeout time.Duration, desired string, terminal ...string) error {
	deadline := time.Now().Add(timeout)
	terminalSet := make(map[string]struct{}, len(terminal))
	for _, state := range terminal {
		terminalSet[state] = struct{}{}
	}
	lastState := ""
	lastReason := ""
	for time.Now().Before(deadline) {
		response, err := r.agrJSON(r.ctx, nil, kind, "get", id, "-o", "json")
		if err == nil {
			var state struct {
				Status       string `json:"Status"`
				StatusReason string `json:"StatusReason"`
				StopReason   string `json:"StopReason"`
			}
			if decodeErr := decodeData(response, &state); decodeErr == nil {
				lastState = state.Status
				lastReason = state.StatusReason
				if lastReason == "" {
					lastReason = state.StopReason
				}
				if lastState == desired {
					return nil
				}
				if _, stopped := terminalSet[lastState]; stopped && lastState != desired {
					return fmt.Errorf("%s %s entered %s while waiting for %s: %s", kind, id, lastState, desired, lastReason)
				}
			}
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return r.ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("%s %s did not reach %s before timeout; last state=%s reason=%s", kind, id, desired, lastState, lastReason)
}

func (r *runner) cleanup() error {
	if r.toolID == "" {
		return nil
	}
	if r.opts.KeepOnFailure && r.failed {
		r.report.Cleanup["tool:"+r.toolID] = "retained"
		return nil
	}
	if err := r.deleteResource("tool", r.toolID); err != nil {
		r.report.Cleanup["tool:"+r.toolID] = "delete failed: " + redact(err.Error())
		return err
	}
	r.report.Cleanup["tool:"+r.toolID] = "deleted"
	return nil
}

func (r *runner) deleteResource(kind, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	arguments := []string{kind, "delete", id}
	if kind == "instance" {
		arguments = append(arguments, "--ignore-not-found")
	} else {
		arguments = append(arguments, "--yes")
	}
	arguments = append(arguments, "-o", "json")
	_, err := r.agrJSON(ctx, nil, arguments...)
	return err
}

func (r *runner) agrJSON(ctx context.Context, request any, arguments ...string) (*agrResult, error) {
	var input []byte
	var err error
	if request != nil {
		input, err = json.Marshal(request)
		if err != nil {
			return nil, err
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	base := []string{"--region", r.env.Region, "--non-interactive", "--no-color"}
	result, err := runAGR(callCtx, r.opts.AGR, input, append(base, arguments...)...)
	if err != nil || !agrSucceeded(result) {
		return result, errors.New(agrFailure(result, err))
	}
	return result, nil
}

func runAGR(ctx context.Context, executable string, input []byte, arguments ...string) (*agrResult, error) {
	command := exec.CommandContext(ctx, executable, arguments...)
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &limitedWriter{Writer: &stdout, Remaining: maxCommandOutput}
	command.Stderr = &limitedWriter{Writer: &stderr, Remaining: maxCommandOutput}
	err := command.Run()
	result := &agrResult{ExitCode: exitCode(command, err), Stderr: redact(stderr.String())}
	if stdout.Len() != 0 {
		if decodeErr := json.Unmarshal(stdout.Bytes(), &result.Envelope); decodeErr != nil && err == nil {
			return result, fmt.Errorf("agr returned invalid JSON: %w", decodeErr)
		}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, err
}

type limitedWriter struct {
	Writer    io.Writer
	Remaining int
}

func (writer *limitedWriter) Write(value []byte) (int, error) {
	original := len(value)
	if len(value) > writer.Remaining {
		value = value[:writer.Remaining]
	}
	if len(value) > 0 {
		_, _ = writer.Writer.Write(value)
		writer.Remaining -= len(value)
	}
	return original, nil
}

func agrSucceeded(result *agrResult) bool {
	return result != nil && result.ExitCode == 0 && result.Envelope.SchemaVersion == "agr.v1" && result.Envelope.Status == "succeeded"
}

func agrFailure(result *agrResult, err error) string {
	if result == nil {
		return redact(errorString(err))
	}
	return redact(fmt.Sprintf("exit=%d error=%s failure=%v stderr=%s", result.ExitCode, errorString(err), result.Envelope.Failure, result.Stderr))
}

func decodeData(result *agrResult, target any) error {
	if result == nil || len(result.Envelope.Data) == 0 {
		return errors.New("agr response has no data")
	}
	return json.Unmarshal(result.Envelope.Data, target)
}

func failureCode(result *agrResult) string {
	if result == nil || result.Envelope.Failure == nil {
		return ""
	}
	failure, ok := result.Envelope.Failure.(map[string]any)
	if !ok {
		return ""
	}
	code, _ := failure["Code"].(string)
	return code
}

type proxyHandle struct {
	port   int
	cancel context.CancelFunc
	wait   <-chan error
	once   sync.Once
}

func (r *runner) startProxy(instanceID string, remotePort int) (*proxyHandle, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	ctx, cancel := context.WithCancel(r.ctx)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := exec.CommandContext(ctx, r.opts.AGR,
		"--region", r.env.Region,
		"--non-interactive", "--no-color",
		"instance", "proxy", instanceID, strconv.Itoa(port)+":"+strconv.Itoa(remotePort),
		"--address", "127.0.0.1",
	)
	command.Stdout = &limitedWriter{Writer: &stdout, Remaining: maxCommandOutput}
	command.Stderr = &limitedWriter{Writer: &stderr, Remaining: maxCommandOutput}
	if err := command.Start(); err != nil {
		cancel()
		return nil, err
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	deadline := time.Now().Add(proxyTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String(), "Forwarding from ") {
			return &proxyHandle{port: port, cancel: cancel, wait: wait}, nil
		}
		select {
		case waitErr := <-wait:
			cancel()
			return nil, fmt.Errorf("proxy exited before ready: %s: %s", errorString(waitErr), redact(stderr.String()))
		case <-time.After(100 * time.Millisecond):
		}
	}
	cancel()
	return nil, fmt.Errorf("proxy did not become ready: %s", redact(stderr.String()))
}

func (proxy *proxyHandle) stop() {
	proxy.once.Do(func() {
		proxy.cancel()
		select {
		case <-proxy.wait:
		case <-time.After(5 * time.Second):
		}
	})
}

func fetchSnapshot(ctx context.Context, localPort int, token string) (model.Snapshot, error) {
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		requestCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		snapshot, err := fetchSnapshotOnce(requestCtx, localPort, token)
		cancel()
		if err == nil {
			return snapshot, nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	return model.Snapshot{}, fmt.Errorf("fetch observer snapshot: %w", lastErr)
}

func fetchSnapshotOnce(ctx context.Context, localPort int, token string) (model.Snapshot, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/v1/snapshot", localPort), nil)
	if err != nil {
		return model.Snapshot{}, err
	}
	request.Header.Set(observerTokenHeader, token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return model.Snapshot{}, err
	}
	defer response.Body.Close()
	var snapshot model.Snapshot
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, maxCommandOutput)).Decode(&snapshot)
	if response.StatusCode != http.StatusOK || decodeErr != nil {
		return model.Snapshot{}, fmt.Errorf("HTTP %d: %v", response.StatusCode, decodeErr)
	}
	return snapshot, nil
}

func (r *runner) writeEvidence() (string, error) {
	path := filepath.Join(r.opts.OutputDir, r.report.RunID+".json")
	contents, err := json.MarshalIndent(r.report, "", "  ")
	if err != nil {
		return "", err
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(r.opts.OutputDir, ".sandcamp-e2e-*.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", err
	}
	return path, nil
}

func newID(prefix string) string {
	return fmt.Sprintf("%s-%s-%s", prefix, time.Now().UTC().Format("20060102t150405"), randomToken()[:8])
}

func randomToken() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value)
}

func exitCode(command *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func redact(value string) string {
	for _, name := range []string{"TENCENTCLOUD_SECRET_ID", "TENCENTCLOUD_SECRET_KEY", "TENCENTCLOUD_TOKEN"} {
		secret := os.Getenv(name)
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func stringPointer(value string) *string { return &value }
func boolPointer(value bool) *bool       { return &value }

func missingImages(items []scenario.Scenario, images scenario.Images) []string {
	available := map[string]bool{
		"fastapi": images.FastAPI != "",
		"egress":  images.Egress != "",
		"nginx":   images.Nginx != "",
		"envd":    images.Envd != "",
	}
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, item := range items {
		for _, required := range item.RequiredImages {
			if !available[required] {
				if _, exists := seen[required]; !exists {
					seen[required] = struct{}{}
					result = append(result, required)
				}
			}
		}
	}
	return result
}

func mainImage(item scenario.Scenario, images scenario.Images) string {
	switch item.MainImage {
	case "", "main":
		return images.Main
	case "nginx":
		return images.Nginx
	default:
		return ""
	}
}
