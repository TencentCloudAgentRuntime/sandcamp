package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TencentCloudAgentRuntime/sandcamp/test/e2e/internal/scenario"
)

func TestLimitedWriterCapsCapturedOutputWithoutBreakingCommand(t *testing.T) {
	var output bytes.Buffer
	writer := &limitedWriter{Writer: &output, Remaining: 4}
	if count, err := writer.Write([]byte("123456")); err != nil || count != 6 {
		t.Fatalf("write = %d, %v", count, err)
	}
	if output.String() != "1234" || writer.Remaining != 0 {
		t.Fatalf("captured %q remaining=%d", output.String(), writer.Remaining)
	}
}

func TestRedactRemovesCredentialValues(t *testing.T) {
	t.Setenv("TENCENTCLOUD_SECRET_ID", "secret-id-value")
	t.Setenv("TENCENTCLOUD_SECRET_KEY", "secret-key-value")
	got := redact("id=secret-id-value key=secret-key-value")
	if strings.Contains(got, "secret-id-value") || strings.Contains(got, "secret-key-value") {
		t.Fatalf("credential remained in %q", got)
	}
}

func TestMissingImagesDependsOnSelectedScenarios(t *testing.T) {
	items := scenario.Core()
	missing := missingImages(items, scenario.Images{})
	joined := strings.Join(missing, ",")
	for _, expected := range []string{"fastapi", "egress", "nginx", "envd"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing %s in %q", expected, joined)
		}
	}
	minimal, _ := scenario.ByName("minimal-root")
	if got := missingImages([]scenario.Scenario{minimal}, scenario.Images{}); len(got) != 0 {
		t.Fatalf("minimal scenario unexpectedly requires %v", got)
	}
}

func TestMainImageSelection(t *testing.T) {
	images := scenario.Images{Main: "main-image", Nginx: "nginx-image"}
	if got := mainImage(scenario.Scenario{}, images); got != "main-image" {
		t.Fatalf("default main image = %q", got)
	}
	if got := mainImage(scenario.Scenario{MainImage: "nginx"}, images); got != "nginx-image" {
		t.Fatalf("Nginx main image = %q", got)
	}
}

func TestParseOptionsNeverDefaultsToCloudAll(t *testing.T) {
	opts, err := parseOptions([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Scenario != "minimal-root" {
		t.Fatalf("default scenario = %q", opts.Scenario)
	}
}

func TestSelectScenariosPreservesExplicitOrderAndRejectsDuplicates(t *testing.T) {
	selected, err := selectScenarios("main-init-job,sidecar-only")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0].Name != "main-init-job" || selected[1].Name != "sidecar-only" {
		t.Fatalf("selected = %#v", selected)
	}
	if _, err := selectScenarios("sidecar-only,sidecar-only"); err == nil {
		t.Fatal("duplicate selection was accepted")
	}
}

func TestFailureCode(t *testing.T) {
	result := &agrResult{Envelope: agrEnvelope{Failure: map[string]any{"Code": "FailedOperation.ContainerStart"}}}
	if got := failureCode(result); got != "FailedOperation.ContainerStart" {
		t.Fatalf("failure code = %q", got)
	}
	if got := failureCode(nil); got != "" {
		t.Fatalf("nil failure code = %q", got)
	}
}

func TestExpectedRejectionCodeIsNarrow(t *testing.T) {
	for _, code := range []string{
		"FailedOperation.ContainerStart",
		"FailedOperation.ContainerProbe",
		"FailedOperation.Timeout",
	} {
		if !expectedRejectionCode(code) {
			t.Errorf("expected rejection code %q was rejected", code)
		}
	}
	for _, code := range []string{"", "AuthFailure.SignatureFailure", "InternalError"} {
		if expectedRejectionCode(code) {
			t.Errorf("unrelated failure code %q was accepted", code)
		}
	}
}
