//go:build !no_yms_rca

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/daemon"
)

// TestParseAndResolve_NoCaptureSecretsEndToEnd wires the CLI parser
// straight into daemon.Resolve to prove that --no-capture-secrets
// actually keeps the yms-rca-discovered token out of the resulting
// EnvExtra (and the default install path includes it). It exercises
// the discoverer registered by plugin_agent_yms_rca.go's init() and
// therefore lives behind the !no_yms_rca build tag.
func TestParseAndResolve_NoCaptureSecretsEndToEnd(t *testing.T) {
	os.Unsetenv("CC_DAEMON_NO_CAPTURE_SECRETS")

	profileDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDir, "yms-dev.yaml"),
		[]byte("mcp:\n  token_env: E2E_PROFILE_TOK\n"), 0o600); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	t.Setenv("CC_YMS_RCA_CONNECTIONS_DIR", profileDir)
	t.Setenv("E2E_PROFILE_TOK", "real-token-value")
	t.Setenv("HTTPS_PROXY", "http://1.2.3.4:8080")

	// Default install (no --no-capture-secrets, env unset) → token captured.
	cfg, _, err := parseDaemonInstallArgs([]string{"--force"})
	if err != nil {
		t.Fatalf("parse default: %v", err)
	}
	cfg.BinaryPath = "/bin/true"
	cfg.WorkDir = t.TempDir()
	if err := daemon.Resolve(&cfg); err != nil {
		t.Fatalf("Resolve default: %v", err)
	}
	if cfg.EnvExtra["E2E_PROFILE_TOK"] != "real-token-value" {
		t.Errorf("default install must capture token; EnvExtra=%+v", cfg.EnvExtra)
	}
	if cfg.EnvExtra["HTTPS_PROXY"] != "http://1.2.3.4:8080" {
		t.Errorf("HTTPS_PROXY must always be captured; EnvExtra=%+v", cfg.EnvExtra)
	}

	// --no-capture-secrets → token NOT captured, proxy still captured.
	cfg2, _, err := parseDaemonInstallArgs([]string{"--no-capture-secrets", "--force"})
	if err != nil {
		t.Fatalf("parse opt-out: %v", err)
	}
	cfg2.BinaryPath = "/bin/true"
	cfg2.WorkDir = t.TempDir()
	if err := daemon.Resolve(&cfg2); err != nil {
		t.Fatalf("Resolve opt-out: %v", err)
	}
	if _, present := cfg2.EnvExtra["E2E_PROFILE_TOK"]; present {
		t.Errorf("--no-capture-secrets must skip token; EnvExtra=%+v", cfg2.EnvExtra)
	}
	if cfg2.EnvExtra["HTTPS_PROXY"] != "http://1.2.3.4:8080" {
		t.Errorf("proxy must still be captured under --no-capture-secrets; EnvExtra=%+v", cfg2.EnvExtra)
	}

	// CC_DAEMON_NO_CAPTURE_SECRETS=1 (no flag) → same as opt-out.
	t.Setenv("CC_DAEMON_NO_CAPTURE_SECRETS", "1")
	cfg3, _, err := parseDaemonInstallArgs([]string{"--force"})
	if err != nil {
		t.Fatalf("parse env-opt-out: %v", err)
	}
	cfg3.BinaryPath = "/bin/true"
	cfg3.WorkDir = t.TempDir()
	if err := daemon.Resolve(&cfg3); err != nil {
		t.Fatalf("Resolve env-opt-out: %v", err)
	}
	if _, present := cfg3.EnvExtra["E2E_PROFILE_TOK"]; present {
		t.Errorf("CC_DAEMON_NO_CAPTURE_SECRETS=1 must skip token; EnvExtra=%+v", cfg3.EnvExtra)
	}
}
