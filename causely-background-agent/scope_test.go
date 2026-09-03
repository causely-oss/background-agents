package main

import "testing"

func TestInScope(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		payload TriggerPayload
		want    bool
	}{
		{
			name:    "no scope signals configured, no label present: accept (preserves single-remediator behavior)",
			cfg:     Config{GitHubRepo: "org/repo"},
			payload: TriggerPayload{},
			want:    true,
		},
		{
			name:    "github-repo label matches configured repo: accept",
			cfg:     Config{GitHubRepo: "org/repo"},
			payload: TriggerPayload{GitHubRepoLabel: "org/repo"},
			want:    true,
		},
		{
			name:    "github-repo label matches case-insensitively: accept",
			cfg:     Config{GitHubRepo: "org/repo"},
			payload: TriggerPayload{GitHubRepoLabel: "Org/Repo"},
			want:    true,
		},
		{
			name:    "github-repo label does not match configured repo: reject",
			cfg:     Config{GitHubRepo: "org/repo"},
			payload: TriggerPayload{GitHubRepoLabel: "org/other-repo"},
			want:    false,
		},
		{
			name:    "scope namespaces configured, entity namespace matches: accept",
			cfg:     Config{GitHubRepo: "org/repo", ScopeNamespaces: []string{"team-a", "staging"}},
			payload: TriggerPayload{EntityNamespace: "team-a"},
			want:    true,
		},
		{
			name:    "scope namespaces configured, matches case-insensitively: accept",
			cfg:     Config{GitHubRepo: "org/repo", ScopeNamespaces: []string{"Team-a"}},
			payload: TriggerPayload{EntityNamespace: "team-a"},
			want:    true,
		},
		{
			name:    "scope namespaces configured, entity namespace does not match: reject",
			cfg:     Config{GitHubRepo: "org/repo", ScopeNamespaces: []string{"team-a"}},
			payload: TriggerPayload{EntityNamespace: "monitoring"},
			want:    false,
		},
		{
			name:    "scope namespaces configured, entity namespace missing entirely: reject",
			cfg:     Config{GitHubRepo: "org/repo", ScopeNamespaces: []string{"team-a"}},
			payload: TriggerPayload{},
			want:    false,
		},
		{
			name: "both signals configured and satisfied: accept",
			cfg:  Config{GitHubRepo: "org/repo", ScopeNamespaces: []string{"team-a"}},
			payload: TriggerPayload{
				GitHubRepoLabel: "org/repo",
				EntityNamespace: "team-a",
			},
			want: true,
		},
		{
			name: "repo label satisfied but namespace not: reject (both must pass)",
			cfg:  Config{GitHubRepo: "org/repo", ScopeNamespaces: []string{"team-a"}},
			payload: TriggerPayload{
				GitHubRepoLabel: "org/repo",
				EntityNamespace: "monitoring",
			},
			want: false,
		},
		{
			name: "regression: root cause on an out-of-scope entity must be rejected even with no repo label",
			cfg:  Config{GitHubRepo: "org/repo", ScopeNamespaces: []string{"team-a"}},
			payload: TriggerPayload{
				EntityName:      "monitoring/some-service",
				EntityNamespace: "monitoring",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := inScope(tt.cfg, tt.payload)
			if got != tt.want {
				t.Errorf("inScope() = %v (reason: %q), want %v", got, reason, tt.want)
			}
			if !got && reason == "" {
				t.Error("inScope() returned false but no reason was given")
			}
		})
	}
}
