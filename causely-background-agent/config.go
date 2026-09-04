package main

import (
	"fmt"
	"time"

	"github.com/ilyakaznacheev/cleanenv"
)

// Config holds causely-background-agent's full configuration: non-secret settings
// come from the mounted config.yaml (rendered from a ConfigMap), while
// credentials come from environment variables backed by a Kubernetes Secret
// and are never written to config.yaml.
type Config struct {
	Port            string   `yaml:"port" env-default:"8080"`
	CauselyMCPURL   string   `yaml:"causely_mcp_url" env-default:"http://portal.causely.localhost/mcp"`
	GitHubRepo      string   `yaml:"github_repo" env-required:"true"` // the one "org/repo" this instance is allowed to read/write
	ScopeNamespaces []string `yaml:"scope_namespaces"`                // optional allowlist of entity namespaces this instance handles

	MaxCostUSD       float64 `yaml:"max_cost_usd" env-default:"5"`         // per-investigation budget cap; <= 0 disables the cap
	MaxWeeklyCostUSD float64 `yaml:"max_weekly_cost_usd" env-default:"50"` // rolling 7-day aggregate cap across all investigations; <= 0 disables it
	CostStateFile    string  `yaml:"cost_state_file"`                      // optional path to persist weekly spend across restarts

	// ActionMode gates whether a finding actually opens a GitHub PR / posts to
	// Slack ("act") or only runs the investigation and records what it would
	// have done ("observe", the default). Defaults to observe so that
	// aggressive experimentation doesn't spam the target repo/Slack channel —
	// flip to "act" deliberately once a config is trusted.
	ActionMode string `yaml:"action_mode" env-default:"observe"`

	// InvestigationRecordPath, if set, appends one JSON-lines InvestigationRecord
	// per run (see investigation_record.go) — the dataset behind every "how well
	// is this working" question, independent of the ephemeral Slack/log output.
	InvestigationRecordPath string `yaml:"investigation_record_path"`

	// Poll is an alternative/additional trigger source to the push webhook: on
	// an interval, ask Causely for open issues directly instead of waiting for
	// a one-time webhook fire — see poll.go. A push notification only reflects
	// the root cause's state at detection time; polling lets the agent observe
	// how it evolves (more/fewer symptoms, cleared, recurred).
	Poll PollConfig `yaml:"poll"`

	// Extra MCP servers beyond the built-in Causely one (e.g. Grafana). Resolved
	// together with CauselyMCPURL/CauselyMCPToken into the final list in loadConfig.
	MCPServers []MCPServerConfig `yaml:"mcp_servers"`

	// Secrets — environment variables backed by a Kubernetes Secret, deliberately
	// carrying no yaml tag so they can never be populated from config.yaml.
	AnthropicKey       string `env:"ANTHROPIC_API_KEY" env-required:"true"`
	GitHubToken        string `env:"GITHUB_TOKEN" env-required:"true"`
	SlackBotToken      string `env:"SLACK_BOT_TOKEN" env-required:"true"`
	SlackSigningSecret string `env:"SLACK_SIGNING_SECRET"`
	CauselyMCPToken    string `env:"CAUSELY_MCP_TOKEN"`
	// CauselyMCPClientID/CauselyMCPClientSecret are an alternative to
	// CauselyMCPToken for a Causely MCP server that expects HTTP Basic
	// credentials (client_id:secret) instead of a bearer token — see
	// MCPServerConfig in mcp_config.go. Both must be set together, or neither.
	CauselyMCPClientID     string `env:"CAUSELY_MCP_CLIENT_ID"`
	CauselyMCPClientSecret string `env:"CAUSELY_MCP_CLIENT_SECRET"`
	// TriggerSharedSecret, if set, is required as a Bearer token on POST
	// /trigger — this agent is a standalone deployable asset reachable over the
	// network, not a same-repo internal call, so unlike the old design it can't
	// lean on network topology alone for protection.
	TriggerSharedSecret string `env:"TRIGGER_SHARED_SECRET"`
}

// PollConfig configures the poll-based trigger source (see poll.go).
type PollConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Interval string `yaml:"interval" env-default:"5m"` // parsed with time.ParseDuration
	// StateFile persists the watermark of already-seen root-cause occurrences
	// across restarts, so a restart doesn't either replay everything or skip
	// everything that arrived while the process was down.
	StateFile string `yaml:"state_file"`
	// EntityNamespace/GitHubRepoLabel mirror the webhook payload's scope hints —
	// polling has no mediator to populate them from entity labels, so they're
	// applied directly to every issue this poll loop fetches.
	EntityNamespace string `yaml:"entity_namespace"`
	GitHubRepoLabel string `yaml:"github_repo_label"`
}

// loadConfig reads Config from the YAML file at path plus environment
// variables, then resolves the final MCP server list — the built-in Causely
// server plus whatever extra servers config.yaml's mcp_servers lists.
func loadConfig(path string) (Config, error) {
	var cfg Config
	if err := cleanenv.ReadConfig(path, &cfg); err != nil {
		return Config{}, err
	}

	// cleanenv's env-required only checks that an env-tagged field's variable is
	// present in the environment, not that its value is non-empty — a Secret
	// whose keys exist but hold "" (e.g. values.yaml's untouched defaults) would
	// otherwise pass silently and only fail later, mid-investigation, with a
	// confusing 401 from Anthropic/GitHub/Slack instead of a clear startup error.
	if cfg.AnthropicKey == "" || cfg.GitHubToken == "" || cfg.SlackBotToken == "" {
		return Config{}, fmt.Errorf("required secrets missing or empty: ANTHROPIC_API_KEY, GITHUB_TOKEN, SLACK_BOT_TOKEN")
	}

	if cfg.ActionMode != actionModeObserve && cfg.ActionMode != actionModeAct {
		return Config{}, fmt.Errorf("action_mode must be %q or %q, got %q", actionModeObserve, actionModeAct, cfg.ActionMode)
	}

	if cfg.Poll.Enabled {
		if _, err := time.ParseDuration(cfg.Poll.Interval); err != nil {
			return Config{}, fmt.Errorf("poll.interval: %w", err)
		}
	}

	if (cfg.CauselyMCPClientID == "") != (cfg.CauselyMCPClientSecret == "") {
		return Config{}, fmt.Errorf("CAUSELY_MCP_CLIENT_ID and CAUSELY_MCP_CLIENT_SECRET must both be set, or neither")
	}

	servers, err := resolveMCPServers(cfg.CauselyMCPURL, cfg.CauselyMCPToken, cfg.CauselyMCPClientID, cfg.CauselyMCPClientSecret, cfg.MCPServers)
	if err != nil {
		return Config{}, err
	}
	cfg.MCPServers = servers

	return cfg, nil
}
