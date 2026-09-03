package main

import (
	"fmt"
	"strings"
)

// MCPServerConfig describes one MCP server the agent should pull tools from.
// Name becomes the tool-name prefix Claude sees (e.g. "grafana__query_range"),
// so it must be unique across all configured servers. Description is optional
// free text surfaced in the system prompt so Claude knows what each source is
// for, beyond what individual tool descriptions say.
//
// Token is set here for the built-in Causely server (from the CAUSELY_MCP_TOKEN
// secret). For additional servers listed in config.yaml's mcp_servers, Token
// lands in the ConfigMap — fine for servers with no auth, but operators adding
// an authenticated extra server should be aware it isn't secret-backed today.
type MCPServerConfig struct {
	Name        string `yaml:"name"`
	URL         string `yaml:"url"`
	Token       string `yaml:"token,omitempty"`
	Description string `yaml:"description,omitempty"`
}

// resolveMCPServers builds the final, validated list of MCP servers: the
// built-in Causely server plus any additional servers from config.yaml's
// mcp_servers list.
func resolveMCPServers(causelyURL, causelyToken string, extra []MCPServerConfig) ([]MCPServerConfig, error) {
	servers := []MCPServerConfig{{
		Name:        "causely",
		URL:         causelyURL,
		Token:       causelyToken,
		Description: "Causely's root-cause analysis, topology, and observability data (logs, metrics, SLOs, defects).",
	}}
	servers = append(servers, extra...)

	if err := validateMCPServers(servers); err != nil {
		return nil, err
	}
	return servers, nil
}

// validateMCPServers rejects configs that would produce ambiguous or broken
// tool routing: every server needs a name and URL, and names must be unique
// (case-insensitively) since they're used as the tool-name prefix.
func validateMCPServers(servers []MCPServerConfig) error {
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		name := strings.ToLower(strings.TrimSpace(s.Name))
		if name == "" {
			return fmt.Errorf("mcp server config missing a name (url=%q)", s.URL)
		}
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("mcp server %q missing a url", s.Name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate mcp server name %q — names must be unique, they're used as the tool-name prefix", s.Name)
		}
		seen[name] = true
	}
	return nil
}
