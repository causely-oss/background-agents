package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	runtimeconfig "github.com/kagent-dev/kagent/go/adk/pkg/config"
	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"gopkg.in/yaml.v3"
)

const (
	defaultAnthropicMaxTokens = 16384
	defaultOpenAIMaxTokens    = 32768
	defaultOpenAIBaseURL      = "https://api.openai.com/v1"
)

// hermesConfig is the subset of Hermes Agent's config.yaml (hermes-agent
// v0.23.3, hermes_cli/config.py DEFAULT_CONFIG) that this harness writes.
// Every key here was checked against the hermes-agent source rather than
// guessed; see README "How the adapter maps kagent config onto Hermes".
type hermesConfig struct {
	// ConfigVersion is Hermes' config schema version. Without it Hermes
	// treats the file as one that predates versioning.
	ConfigVersion int                        `yaml:"_config_version"`
	Model         hermesModel                `yaml:"model"`
	Providers     map[string]hermesProvider  `yaml:"providers,omitempty"`
	MCPServers    map[string]hermesMCPServer `yaml:"mcp_servers,omitempty"`
	Terminal      hermesTerminal             `yaml:"terminal"`
	Approvals     hermesApprovals            `yaml:"approvals"`
	Skills        hermesSkills               `yaml:"skills"`
	Tools         hermesTools                `yaml:"tools"`
	Agent         hermesAgent                `yaml:"agent"`
}

// hermesAgent caps model iterations per turn. Hermes 0.23 defaults to no
// cap; 90 is the limit Hermes' ACP path had before, and part of the spend
// bound documented in the operator guide.
type hermesAgent struct {
	MaxTurns int `yaml:"max_turns"`
}

const hermesMaxTurns = 90

// hermesConfigVersion is DEFAULT_CONFIG["_config_version"] in the pinned
// Hermes (hermes_cli/config_defaults.py).
const hermesConfigVersion = 54

// hermesTools turns off Hermes' tool search, which otherwise hides some tools
// (MCP tools included) behind a tool_search call when there are many. The
// prompt and skill name the Causely tools directly, so they must be visible.
type hermesTools struct {
	ToolSearch struct {
		Enabled string `yaml:"enabled"`
	} `yaml:"tool_search"`
}

type hermesModel struct {
	Default   string `yaml:"default"`
	Provider  string `yaml:"provider"`
	BaseURL   string `yaml:"base_url,omitempty"`
	MaxTokens int    `yaml:"max_tokens"`
}

type hermesProvider struct {
	BaseURL string `yaml:"base_url"`
	KeyEnv  string `yaml:"key_env"`
	APIMode string `yaml:"api_mode"`
	Model   string `yaml:"model"`
}

// hermesMCPServer mirrors one mcp_servers entry (tools/mcp_tool.py). Header
// values may contain ${VAR} references, which Hermes interpolates from the
// process environment at connect time, so credentials never land on disk.
type hermesMCPServer struct {
	URL       string            `yaml:"url"`
	Transport string            `yaml:"transport,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	Timeout   float64           `yaml:"timeout,omitempty"`
	// SSLVerify is passed to httpx as `verify`: false, or a CA bundle path.
	SSLVerify any               `yaml:"ssl_verify,omitempty"`
	Tools     *hermesToolFilter `yaml:"tools,omitempty"`
}

type hermesToolFilter struct {
	Include []string `yaml:"include,omitempty"`
}

type hermesTerminal struct {
	Cwd string `yaml:"cwd"`
}

type hermesApprovals struct {
	// Mode "manual" routes every dangerous command to an ACP permission
	// request, which hermesACPClient always rejects. "smart" would let an
	// auxiliary LLM auto-approve, which we do not want unattended.
	Mode string `yaml:"mode"`
	// Deny is a list of case-insensitive fnmatch globs that block a terminal
	// command unconditionally (tools/approval_floors.py _match_user_deny_rule).
	Deny []string `yaml:"deny,omitempty"`
}

type hermesSkills struct {
	ExternalDirs []string `yaml:"external_dirs,omitempty"`
}

// layout says where on disk the harness materializes Hermes state.
type layout struct {
	DataDir   string // HOME; Hermes home is DataDir/hermes
	Workspace string // Hermes terminal cwd and ACP session cwd
	SkillsDir string // read-only skills baked into the image
}

func (l layout) hermesHome() string { return filepath.Join(l.DataDir, "hermes") }

// decodeAgentConfig parses the BYO configuration kagent compiles for the
// Actor (an ADKConfig: runtime settings plus the agent) with kagent's own
// protobuf contract, and returns the agent part.
func decodeAgentConfig(raw []byte) (*configv1.ADKConfig, error) {
	var input configv1.ADKConfig
	if err := runtimeconfig.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	if input.GetAgent() == nil {
		return nil, fmt.Errorf("the kagent configuration has no agent")
	}
	return &input, nil
}

// materializeHermesConfig writes $HERMES_HOME/config.yaml and
// $HERMES_HOME/SOUL.md, and returns the configuration it wrote.
func materializeHermesConfig(input *configv1.ADKAgentConfig, pol policy, env runtimeEnv, l layout) (*hermesConfig, error) {
	output, err := buildHermesConfig(input, pol, env, l)
	if err != nil {
		return nil, err
	}
	if err := writeHermesFiles(input, pol, env, l, output); err != nil {
		return nil, err
	}
	return output, nil
}

func writeHermesFiles(input *configv1.ADKAgentConfig, pol policy, env runtimeEnv, l layout, output *hermesConfig) error {
	if err := os.MkdirAll(l.hermesHome(), 0o700); err != nil {
		return fmt.Errorf("create Hermes home: %w", err)
	}
	if err := os.MkdirAll(l.Workspace, 0o700); err != nil {
		return fmt.Errorf("create Hermes workspace: %w", err)
	}
	contents, err := yaml.Marshal(output)
	if err != nil {
		return fmt.Errorf("marshal Hermes configuration: %w", err)
	}
	if err := os.WriteFile(filepath.Join(l.hermesHome(), "config.yaml"), contents, 0o600); err != nil {
		return fmt.Errorf("write Hermes configuration: %w", err)
	}
	// Hermes' ACP adapter builds AIAgent without the CLI-only
	// agent.system_prompt key (acp_adapter/session.py _make_agent), so the
	// AgentTemplate instruction must go into SOUL.md, the identity slot
	// every Hermes entry point loads from HERMES_HOME.
	soul := buildSoul(input.GetInstruction(), pol, env)
	if err := os.WriteFile(filepath.Join(l.hermesHome(), "SOUL.md"), []byte(soul), 0o600); err != nil {
		return fmt.Errorf("write Hermes SOUL.md: %w", err)
	}
	return nil
}

func buildHermesConfig(input *configv1.ADKAgentConfig, pol policy, env runtimeEnv, l layout) (*hermesConfig, error) {
	output := &hermesConfig{ConfigVersion: hermesConfigVersion}
	output.Tools.ToolSearch.Enabled = "off"
	output.Agent.MaxTurns = hermesMaxTurns
	if err := mapModel(input.GetModel(), env, output); err != nil {
		return nil, err
	}
	servers, err := mapMCPServers(input.GetHttpTools(), input.GetSseTools())
	if err != nil {
		return nil, err
	}
	output.MCPServers = servers
	output.Terminal.Cwd = l.Workspace
	output.Approvals.Mode = "manual"
	output.Approvals.Deny = denyPatterns(pol)
	if l.SkillsDir != "" {
		output.Skills.ExternalDirs = []string{l.SkillsDir}
	}
	return output, nil
}

func mapModel(model *configv1.ADKModelConfig, env runtimeEnv, output *hermesConfig) error {
	if model == nil {
		return fmt.Errorf("the AgentTemplate has no modelConfig; Hermes needs an Anthropic or OpenAI ModelConfig")
	}
	if model.GetApiKeyPassthrough() {
		return fmt.Errorf("ModelConfig apiKeyPassthrough is not supported by the Hermes harness")
	}
	name := strings.TrimSpace(model.GetModel())
	switch {
	case model.GetAnthropic() != nil:
		m := model.GetAnthropic()
		if name == "" {
			return fmt.Errorf("Anthropic model name is required")
		}
		if !env.AnthropicKey {
			return fmt.Errorf("ANTHROPIC_API_KEY is not set; set apiKeySecret on the Anthropic ModelConfig")
		}
		output.Model = hermesModel{
			Default: name, Provider: "anthropic", BaseURL: m.GetBaseUrl(),
			MaxTokens: firstPositive(m.MaxTokens, nil, defaultAnthropicMaxTokens),
		}
		return nil
	case model.GetOpenai() != nil:
		m := model.GetOpenai()
		if name == "" {
			return fmt.Errorf("OpenAI model name is required")
		}
		if !env.OpenAIKey {
			return fmt.Errorf("OPENAI_API_KEY is not set; set apiKeySecret on the OpenAI ModelConfig")
		}
		baseURL := m.GetBaseUrl()
		if baseURL == "" {
			baseURL = defaultOpenAIBaseURL
		}
		output.Model = hermesModel{
			Default: name, Provider: "openai", BaseURL: baseURL,
			MaxTokens: firstPositive(m.MaxCompletionTokens, m.MaxTokens, defaultOpenAIMaxTokens),
		}
		apiMode := "chat_completions"
		if m.GetApiFormat() == "responses" {
			apiMode = "codex_responses"
		}
		output.Providers = map[string]hermesProvider{"openai": {
			BaseURL: baseURL, KeyEnv: "OPENAI_API_KEY", APIMode: apiMode, Model: name,
		}}
		return nil
	default:
		return fmt.Errorf("the Hermes harness supports Anthropic and OpenAI ModelConfigs, got %T", model.GetProvider())
	}
}

func firstPositive(a, b *int64, fallback int) int {
	for _, v := range []*int64{a, b} {
		if v != nil && *v > 0 {
			return int(*v)
		}
	}
	return fallback
}

// kagentEnvPlaceholder is how kagent's adkconfig builder encodes a
// Secret-backed MCP header: the literal value is replaced by
// __KAGENT_ENV[NAME]__ and NAME is injected into the Actor environment.
var kagentEnvPlaceholder = regexp.MustCompile(`__KAGENT_ENV\[([A-Za-z_][A-Za-z0-9_]*)\]__`)

// hermesSecretEnvName is the name the adapter gives a kagent credential
// variable in the Hermes process environment. Hermes strips
// AUXILIARY_*_API_KEY from every subprocess it spawns, unconditionally
// (tools/environments/local_env_policy.py _is_hermes_internal_secret), so the MCP
// credential is usable for ${...} header interpolation but not inherited by
// the terminal tool. A plain KAGENT_CREDENTIAL_* name would be inherited by
// every command the model runs.
func hermesSecretEnvName(name string) string { return "AUXILIARY_" + name + "_API_KEY" }

func translateHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		out[k] = kagentEnvPlaceholder.ReplaceAllStringFunc(v, func(m string) string {
			return "${" + hermesSecretEnvName(kagentEnvPlaceholder.FindStringSubmatch(m)[1]) + "}"
		})
	}
	return out
}

// headerSecretNames lists the environment variables kagent injected for
// Secret-backed MCP headers.
func headerSecretNames(input *configv1.ADKAgentConfig) []string {
	var names []string
	collect := func(headers map[string]string) {
		for _, v := range headers {
			for _, m := range kagentEnvPlaceholder.FindAllStringSubmatch(v, -1) {
				names = append(names, m[1])
			}
		}
	}
	for _, t := range input.GetHttpTools() {
		collect(t.GetParams().GetHeaders())
	}
	for _, t := range input.GetSseTools() {
		collect(t.GetParams().GetHeaders())
	}
	return names
}

func sslVerify(insecure *bool, caPath *string) any {
	if caPath != nil && *caPath != "" {
		return *caPath
	}
	if insecure != nil && *insecure {
		return false
	}
	return nil
}

func mapMCPServers(httpTools []*configv1.ADKHTTPMCPServerConfig, sseTools []*configv1.ADKSSEMCPServerConfig) (map[string]hermesMCPServer, error) {
	if len(httpTools)+len(sseTools) == 0 {
		return nil, nil
	}
	servers := map[string]hermesMCPServer{}
	add := func(rawURL string, server hermesMCPServer, tools []string) error {
		name, err := mcpServerName(rawURL, servers)
		if err != nil {
			return err
		}
		server.URL = rawURL
		if len(tools) > 0 {
			server.Tools = &hermesToolFilter{Include: append([]string(nil), tools...)}
		}
		servers[name] = server
		return nil
	}
	for _, tool := range httpTools {
		p := tool.GetParams()
		server := hermesMCPServer{Headers: translateHeaders(p.GetHeaders()), SSLVerify: sslVerify(p.TlsInsecureSkipVerify, p.TlsCaCertPath)}
		if t := p.GetTimeout().AsDuration().Seconds(); t > 0 {
			server.Timeout = t
		}
		if err := add(p.GetUrl(), server, tool.GetTools()); err != nil {
			return nil, err
		}
	}
	for _, tool := range sseTools {
		p := tool.GetParams()
		server := hermesMCPServer{Transport: "sse", Headers: translateHeaders(p.GetHeaders()), SSLVerify: sslVerify(p.TlsInsecureSkipVerify, p.TlsCaCertPath)}
		if t := p.GetTimeout().AsDuration().Seconds(); t > 0 {
			server.Timeout = t
		}
		if err := add(p.GetUrl(), server, tool.GetTools()); err != nil {
			return nil, err
		}
	}
	return servers, nil
}

var nonIdentifier = regexp.MustCompile(`[^a-z0-9]+`)

// mcpServerName derives a stable, readable Hermes server name from an MCP
// URL. kagent's AgentConfig does not carry the RemoteMCPServer's name, and
// Hermes prefixes every tool with it (mcp__<server>__<tool>), so
// https://api.causely.app/mcp becomes "causely" and an in-cluster
// http://causely-mcp.causely.svc:8080/mcp becomes "causely_mcp".
func mcpServerName(rawURL string, used map[string]hermesMCPServer) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return "", fmt.Errorf("MCP server URL %q must be an absolute http(s) URL", rawURL)
	}
	labels := strings.Split(strings.ToLower(parsed.Hostname()), ".")
	base := labels[0]
	for _, label := range labels {
		if label == "api" || label == "mcp" || label == "www" {
			continue
		}
		base = label
		break
	}
	base = strings.Trim(nonIdentifier.ReplaceAllString(base, "_"), "_")
	if base == "" || (base[0] >= '0' && base[0] <= '9') {
		base = "mcp_" + base
	}
	name := base
	for i := 2; ; i++ {
		if _, taken := used[name]; !taken {
			return name, nil
		}
		name = fmt.Sprintf("%s_%d", base, i)
	}
}
