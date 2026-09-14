package main

import (
	"strings"
	"testing"
)

func TestResolveMCPServers_CauselyOnlyByDefault(t *testing.T) {
	servers, err := resolveMCPServers("http://causely.local/mcp", "", "", "", nil)
	if err != nil {
		t.Fatalf("resolveMCPServers() error = %v", err)
	}
	if len(servers) != 1 || servers[0].Name != "causely" || servers[0].URL != "http://causely.local/mcp" {
		t.Fatalf("resolveMCPServers() = %+v, want a single causely entry", servers)
	}
}

func TestResolveMCPServers_AddsExtraServers(t *testing.T) {
	extra := []MCPServerConfig{{Name: "grafana", URL: "https://grafana.example.com/mcp", Token: "tok", Description: "dashboards"}}
	servers, err := resolveMCPServers("http://causely.local/mcp", "", "", "", extra)
	if err != nil {
		t.Fatalf("resolveMCPServers() error = %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("resolveMCPServers() = %+v, want 2 servers", servers)
	}
	if servers[1].Name != "grafana" || servers[1].URL != "https://grafana.example.com/mcp" || servers[1].Token != "tok" {
		t.Errorf("grafana server = %+v, not as configured", servers[1])
	}
}

func TestResolveMCPServers_DuplicateNameIsRejected(t *testing.T) {
	extra := []MCPServerConfig{{Name: "causely", URL: "https://other.example.com/mcp"}}
	_, err := resolveMCPServers("http://causely.local/mcp", "", "", "", extra)
	if err == nil {
		t.Fatal("expected an error for a duplicate server name")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %v, want it to mention the duplicate name", err)
	}
}

func TestResolveMCPServers_MissingURLIsRejected(t *testing.T) {
	extra := []MCPServerConfig{{Name: "grafana", URL: ""}}
	_, err := resolveMCPServers("http://causely.local/mcp", "", "", "", extra)
	if err == nil {
		t.Fatal("expected an error for a server missing a url")
	}
}

func TestResolveMCPServers_MissingNameIsRejected(t *testing.T) {
	extra := []MCPServerConfig{{Name: "", URL: "https://grafana.example.com/mcp"}}
	_, err := resolveMCPServers("http://causely.local/mcp", "", "", "", extra)
	if err == nil {
		t.Fatal("expected an error for a server missing a name")
	}
}

func TestResolveMCPServers_NamesAreCaseInsensitivelyUnique(t *testing.T) {
	extra := []MCPServerConfig{{Name: "Causely", URL: "https://other.example.com/mcp"}}
	_, err := resolveMCPServers("http://causely.local/mcp", "", "", "", extra)
	if err == nil {
		t.Fatal("expected an error — 'Causely' collides with 'causely' case-insensitively")
	}
}

func TestResolveMCPServers_CauselyClientCredentialsArePlumbedThrough(t *testing.T) {
	servers, err := resolveMCPServers("http://causely.local/mcp", "", "my-client", "my-secret", nil)
	if err != nil {
		t.Fatalf("resolveMCPServers() error = %v", err)
	}
	if servers[0].ClientID != "my-client" || servers[0].ClientSecret != "my-secret" {
		t.Errorf("causely server = %+v, want client_id/client_secret set", servers[0])
	}
}

func TestResolveMCPServers_PartialClientCredentialsRejected(t *testing.T) {
	extra := []MCPServerConfig{{Name: "grafana", URL: "https://grafana.example.com/mcp", ClientID: "id-only"}}
	_, err := resolveMCPServers("http://causely.local/mcp", "", "", "", extra)
	if err == nil {
		t.Fatal("expected an error for a client_id set without a client_secret")
	}
}

func TestMCPServerConfig_NewClient_PrefersClientCredentialsOverToken(t *testing.T) {
	s := MCPServerConfig{Name: "causely", URL: "http://x", Token: "should-be-ignored", ClientID: "id", ClientSecret: "secret"}
	c := s.newClient()
	if c.token != "" || c.clientID != "id" || c.clientSecret != "secret" {
		t.Errorf("newClient() = %+v, want client_id/client_secret auth with token unset", c)
	}
}

func TestMCPServerConfig_NewClient_FallsBackToToken(t *testing.T) {
	s := MCPServerConfig{Name: "causely", URL: "http://x", Token: "tok"}
	c := s.newClient()
	if c.token != "tok" || c.clientID != "" || c.clientSecret != "" {
		t.Errorf("newClient() = %+v, want bearer token auth", c)
	}
}
