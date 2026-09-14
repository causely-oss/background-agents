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
//
// ClientID/ClientSecret are an alternative to Token for MCP servers that expect
// HTTP Basic credentials instead of a bearer token (e.g. Causely's own MCP
// server on a tenant with Frontegg auth enabled: it exchanges client_id:secret
// for a Frontegg access token itself and caches it, so the caller only ever
// needs to send the same static pair — no token fetch/refresh logic here). If
// both are set, ClientID/ClientSecret take precedence over Token.
type MCPServerConfig struct {
	Name         string `yaml:"name"`
	URL          string `yaml:"url"`
	Token        string `yaml:"token,omitempty"`
	ClientID     string `yaml:"client_id,omitempty"`
	ClientSecret string `yaml:"client_secret,omitempty"`
	Description  string `yaml:"description,omitempty"`
}

// newClient builds the mcpClient for this server, picking Basic-credential auth
// over a bearer token when both a client ID and secret are configured.
func (s MCPServerConfig) newClient() *mcpClient {
	if s.ClientID != "" && s.ClientSecret != "" {
		return newMCPClientBasicAuth(s.URL, s.ClientID, s.ClientSecret)
	}
	return newMCPClient(s.URL, s.Token)
}

// resolveMCPServers builds the final, validated list of MCP servers: the
// built-in Causely server plus any additional servers from config.yaml's
// mcp_servers list.
func resolveMCPServers(causelyURL, causelyToken, causelyClientID, causelyClientSecret string, extra []MCPServerConfig) ([]MCPServerConfig, error) {
	servers := []MCPServerConfig{{
		Name:         "causely",
		URL:          causelyURL,
		Token:        causelyToken,
		ClientID:     causelyClientID,
		ClientSecret: causelyClientSecret,
		Description:  "Causely's root-cause analysis, topology, and observability data (logs, metrics, SLOs, defects).",
	}}
	servers = append(servers, extra...)

	if err := validateMCPServers(servers); err != nil {
		return nil, err
	}
	return servers, nil
}

// validateMCPServers rejects configs that would produce ambiguous or broken
// tool routing or auth: every server needs a name and URL, names must be
// unique (case-insensitively) since they're used as the tool-name prefix, and
// a partially-configured client_id/client_secret pair (only one of the two
// set) would otherwise fail silently by falling back to no-Basic-auth.
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
		if (s.ClientID == "") != (s.ClientSecret == "") {
			return fmt.Errorf("mcp server %q: client_id and client_secret must both be set, or neither", s.Name)
		}
	}
	return nil
}
