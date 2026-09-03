package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}

func setRequiredSecretEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"ANTHROPIC_API_KEY": "ak",
		"GITHUB_TOKEN":      "gt",
		"SLACK_BOT_TOKEN":   "sbt",
	} {
		t.Setenv(k, v)
	}
}

func TestLoadConfig_ParsesYAMLAndMergesMCPServers(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeTestConfig(t, `
github_repo: "org/repo"
scope_namespaces: ["team-a", "team-b"]
max_cost_usd: 7.5
mcp_servers:
  - name: grafana
    url: https://grafana.example.com/mcp
`)

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.GitHubRepo != "org/repo" {
		t.Errorf("GitHubRepo = %q, want %q", cfg.GitHubRepo, "org/repo")
	}
	if len(cfg.ScopeNamespaces) != 2 || cfg.ScopeNamespaces[0] != "team-a" {
		t.Errorf("ScopeNamespaces = %v", cfg.ScopeNamespaces)
	}
	if cfg.MaxCostUSD != 7.5 {
		t.Errorf("MaxCostUSD = %v, want 7.5", cfg.MaxCostUSD)
	}
	if len(cfg.MCPServers) != 2 || cfg.MCPServers[0].Name != "causely" || cfg.MCPServers[1].Name != "grafana" {
		t.Fatalf("MCPServers = %+v, want [causely, grafana]", cfg.MCPServers)
	}
}

func TestLoadConfig_DefaultsApplyWhenUnset(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeTestConfig(t, `github_repo: "org/repo"`)

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want default 8080", cfg.Port)
	}
	if cfg.MaxCostUSD != 5 || cfg.MaxWeeklyCostUSD != 50 {
		t.Errorf("cost defaults = %v/%v, want 5/50", cfg.MaxCostUSD, cfg.MaxWeeklyCostUSD)
	}
	if cfg.ActionMode != actionModeObserve {
		t.Errorf("ActionMode = %q, want default %q", cfg.ActionMode, actionModeObserve)
	}
}

func TestLoadConfig_InvalidActionModeErrors(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeTestConfig(t, "github_repo: \"org/repo\"\naction_mode: \"yolo\"\n")

	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected an error for an invalid action_mode")
	}
}

func TestLoadConfig_PollEnabledWithInvalidIntervalErrors(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeTestConfig(t, "github_repo: \"org/repo\"\npoll:\n  enabled: true\n  interval: \"not-a-duration\"\n")

	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected an error for an unparseable poll.interval")
	}
}

func TestLoadConfig_MissingRequiredGitHubRepoErrors(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeTestConfig(t, `port: "9090"`)

	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected an error when github_repo is missing")
	}
}

func TestLoadConfig_MissingSecretEnvErrors(t *testing.T) {
	path := writeTestConfig(t, `github_repo: "org/repo"`)

	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected an error when required secret env vars are unset")
	}
}

// TestLoadConfig_EmptySecretEnvErrors covers a Secret whose keys exist but hold
// "" (e.g. an untouched values.yaml default) — cleanenv's env-required alone
// only checks the env var is present, not non-empty, so this must be checked
// explicitly or a misconfigured Secret would pass loadConfig silently.
func TestLoadConfig_EmptySecretEnvErrors(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GITHUB_TOKEN", "gt")
	t.Setenv("SLACK_BOT_TOKEN", "sbt")
	path := writeTestConfig(t, `github_repo: "org/repo"`)

	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected an error when a required secret env var is set but empty")
	}
}
