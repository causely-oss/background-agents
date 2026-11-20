package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimeconfig "github.com/kagent-dev/kagent/go/adk/pkg/config"
	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"gopkg.in/yaml.v3"
)

func ptr[T any](v T) *T { return &v }

var allCreds = runtimeEnv{AnthropicKey: true, OpenAIKey: true, GitHubToken: true, Kubectl: true}

// kagentConfigJSON marshals an agent exactly the way kagent's BYO compiler
// does (runtimeconfig.Marshal of an ADKConfig, core/internal/translator/byo),
// so these tests exercise the real wire format rather than an approximation.
func kagentConfigJSON(t *testing.T, agent *configv1.ADKAgentConfig) *configv1.ADKAgentConfig {
	t.Helper()
	raw, err := runtimeconfig.Marshal(&configv1.ADKConfig{
		Runtime: &configv1.ADKRuntimeConfig{ControlPlaneUrl: "http://kagent-controller.kagent:8083"},
		Agent:   agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeAgentConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	return decoded.GetAgent()
}

func anthropicModel(name string, maxTokens *int64) *configv1.ADKModelConfig {
	return &configv1.ADKModelConfig{Model: name, Provider: &configv1.ADKModelConfig_Anthropic{Anthropic: &configv1.ADKAnthropic{MaxTokens: maxTokens}}}
}

func openAIModel(name string, m *configv1.ADKOpenAI) *configv1.ADKModelConfig {
	return &configv1.ADKModelConfig{Model: name, Provider: &configv1.ADKModelConfig_Openai{Openai: m}}
}

func readHermesConfig(t *testing.T, l layout) (hermesConfig, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(l.hermesHome(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out hermesConfig
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	soul, err := os.ReadFile(filepath.Join(l.hermesHome(), "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	return out, string(soul)
}

func testLayout(t *testing.T) layout {
	root := t.TempDir()
	return layout{DataDir: root, Workspace: filepath.Join(root, "workspace"), SkillsDir: "/opt/causely/skills"}
}

func TestMaterializeAnthropicWithCauselyMCP(t *testing.T) {
	t.Parallel()
	l := testLayout(t)
	raw := kagentConfigJSON(t, &configv1.ADKAgentConfig{
		Model:       anthropicModel("claude-opus-5-5", ptr[int64](8000)),
		Instruction: "You are an SRE agent.",
		HttpTools: []*configv1.ADKHTTPMCPServerConfig{{
			Params: &configv1.ADKHTTPConnectionParams{
				Url:     "https://api.causely.app/mcp",
				Headers: map[string]string{"Authorization": "Bearer __KAGENT_ENV[KAGENT_CREDENTIAL_ABC123]__", "X-Static": "v"},
				Timeout: durationpb.New(120 * time.Second),
			},
			Tools: []string{"get_issue_details", "get_logs"},
		}},
		SseTools: []*configv1.ADKSSEMCPServerConfig{{
			Params: &configv1.ADKSSEConnectionParams{Url: "http://grafana-mcp.monitoring.svc:8000/sse", TlsInsecureSkipVerify: ptr(true)},
		}},
	})
	if _, err := materializeHermesConfig(raw, policy{ActionMode: actionModeObserve, GitHubRepo: "acme/shop"}, allCreds, l); err != nil {
		t.Fatal(err)
	}
	cfg, soul := readHermesConfig(t, l)

	if cfg.ConfigVersion != hermesConfigVersion || cfg.Tools.ToolSearch.Enabled != "off" || cfg.Agent.MaxTurns != 90 {
		t.Fatalf("config version %d, tool_search %q, max_turns %d", cfg.ConfigVersion, cfg.Tools.ToolSearch.Enabled, cfg.Agent.MaxTurns)
	}
	if cfg.Model != (hermesModel{Default: "claude-opus-5-5", Provider: "anthropic", MaxTokens: 8000}) {
		t.Fatalf("model: %+v", cfg.Model)
	}
	if cfg.Providers != nil {
		t.Fatalf("anthropic needs no custom provider entry, got %+v", cfg.Providers)
	}
	causely, ok := cfg.MCPServers["causely"]
	if !ok {
		t.Fatalf("expected server named causely, got %v", cfg.MCPServers)
	}
	if got := causely.Headers["Authorization"]; got != "Bearer ${AUXILIARY_KAGENT_CREDENTIAL_ABC123_API_KEY}" {
		t.Fatalf("secret header not translated to Hermes interpolation: %q", got)
	}
	if causely.Headers["X-Static"] != "v" || causely.Timeout != 120 || causely.Transport != "" {
		t.Fatalf("causely server: %+v", causely)
	}
	if causely.Tools == nil || strings.Join(causely.Tools.Include, ",") != "get_issue_details,get_logs" {
		t.Fatalf("tool allowlist not mapped: %+v", causely.Tools)
	}
	grafana, ok := cfg.MCPServers["grafana_mcp"]
	if !ok || grafana.Transport != "sse" || grafana.SSLVerify != false || grafana.Tools != nil {
		t.Fatalf("grafana server: %+v (all: %v)", grafana, cfg.MCPServers)
	}

	if names := headerSecretNames(raw); strings.Join(names, ",") != "KAGENT_CREDENTIAL_ABC123" {
		t.Fatalf("header secret names: %v", names)
	}
	if cfg.Terminal.Cwd != l.Workspace {
		t.Fatalf("terminal: %+v", cfg.Terminal)
	}
	if cfg.Approvals.Mode != "manual" || len(cfg.Approvals.Deny) == 0 {
		t.Fatalf("approvals: %+v", cfg.Approvals)
	}
	if strings.Join(cfg.Skills.ExternalDirs, ",") != "/opt/causely/skills" {
		t.Fatalf("skills: %+v", cfg.Skills)
	}

	if !strings.HasPrefix(soul, "You are an SRE agent.") || !strings.Contains(soul, "Action mode: OBSERVE") || !strings.Contains(soul, "acme/shop") {
		t.Fatalf("SOUL.md:\n%s", soul)
	}
}

func TestMaterializeWritesNoSecretValues(t *testing.T) {
	t.Parallel()
	l := testLayout(t)
	raw := kagentConfigJSON(t, &configv1.ADKAgentConfig{
		Model: anthropicModel("claude-opus-5-5", nil),
		HttpTools: []*configv1.ADKHTTPMCPServerConfig{{Params: &configv1.ADKHTTPConnectionParams{
			Url: "https://api.causely.app/mcp", Headers: map[string]string{"Authorization": "__KAGENT_ENV[TOKEN_ENV]__"},
		}}},
	})
	if _, err := materializeHermesConfig(raw, policy{ActionMode: actionModeObserve}, allCreds, l); err != nil {
		t.Fatal(err)
	}
	contents, _ := os.ReadFile(filepath.Join(l.hermesHome(), "config.yaml"))
	if strings.Contains(string(contents), "__KAGENT_ENV") || !strings.Contains(string(contents), "${AUXILIARY_TOKEN_ENV_API_KEY}") {
		t.Fatalf("config.yaml should reference the env var, not a placeholder:\n%s", contents)
	}
	info, err := os.Stat(filepath.Join(l.hermesHome(), "config.yaml"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config.yaml mode: %v %v", info.Mode(), err)
	}
}

func TestMapModelOpenAI(t *testing.T) {
	t.Parallel()
	out := &hermesConfig{}
	err := mapModel(openAIModel("gpt-5", &configv1.ADKOpenAI{MaxTokens: ptr[int64](1000), MaxCompletionTokens: ptr[int64](2000), ApiFormat: "responses"}), allCreds, out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != (hermesModel{Default: "gpt-5", Provider: "openai", BaseURL: defaultOpenAIBaseURL, MaxTokens: 2000}) {
		t.Fatalf("model: %+v", out.Model)
	}
	if p := out.Providers["openai"]; p.KeyEnv != "OPENAI_API_KEY" || p.APIMode != "codex_responses" || p.Model != "gpt-5" {
		t.Fatalf("provider: %+v", p)
	}
}

func TestMapModelErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		model *configv1.ADKModelConfig
		env   runtimeEnv
		want  string
	}{
		"no model":          {nil, allCreds, "no modelConfig"},
		"unsupported":       {&configv1.ADKModelConfig{Model: "gemini", Provider: &configv1.ADKModelConfig_Gemini{Gemini: &configv1.ADKGemini{}}}, allCreds, "supports Anthropic and OpenAI"},
		"anthropic no key":  {anthropicModel("claude", nil), runtimeEnv{}, "ANTHROPIC_API_KEY"},
		"openai no key":     {openAIModel("gpt", &configv1.ADKOpenAI{}), runtimeEnv{}, "OPENAI_API_KEY"},
		"anthropic no name": {anthropicModel("", nil), allCreds, "model name is required"},
		"key passthrough":   {&configv1.ADKModelConfig{Model: "claude", ApiKeyPassthrough: true, Provider: &configv1.ADKModelConfig_Anthropic{Anthropic: &configv1.ADKAnthropic{}}}, allCreds, "apiKeyPassthrough"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := mapModel(tc.model, tc.env, &hermesConfig{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestDecodeRejectsBadConfig(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"agent":{"model":`, `{"agent":{"unknown_field":1}}`, `{"runtime":{}}`} {
		if _, err := decodeAgentConfig([]byte(raw)); err == nil {
			t.Fatalf("%s: expected decode error", raw)
		}
	}
}

func TestMCPServerName(t *testing.T) {
	t.Parallel()
	used := map[string]hermesMCPServer{}
	for _, tc := range []struct{ url, want string }{
		{"https://api.causely.app/mcp", "causely"},
		{"https://mcp.causely.app/mcp", "causely_2"},
		{"http://causely-mcp.causely.svc.cluster.local:8080/mcp", "causely_mcp"},
		{"http://10.0.0.7:9000/mcp", "mcp_10"},
	} {
		got, err := mcpServerName(tc.url, used)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.url, got, tc.want)
		}
		used[got] = hermesMCPServer{}
	}
	if _, err := mcpServerName("file:///etc/passwd", used); err == nil {
		t.Fatal("expected non-http URL to be rejected")
	}
}

func TestSSLVerify(t *testing.T) {
	t.Parallel()
	if v := sslVerify(nil, nil); v != nil {
		t.Fatalf("default should be omitted, got %v", v)
	}
	if v := sslVerify(ptr(true), ptr("/etc/ca.pem")); v != "/etc/ca.pem" {
		t.Fatalf("CA path should win, got %v", v)
	}
}
