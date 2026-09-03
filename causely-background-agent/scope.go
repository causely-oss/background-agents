package main

import (
	"fmt"
	"strings"
)

// inScope reports whether this remediator instance may act on the triggering
// root cause. Mediator-side routing is best-effort, not a guarantee, so a
// remediator must not open a PR against a repo it wasn't configured for.
//
// Two independent, non-exclusive signals are checked:
//  1. GitHubRepoLabel — if set, must match this instance's configured GitHubRepo.
//  2. ScopeNamespaces — if configured, the entity's namespace must be in the list.
//
// If neither signal is available, the trigger is accepted by default.
func inScope(cfg Config, payload TriggerPayload) (bool, string) {
	if repo := strings.TrimSpace(payload.GitHubRepoLabel); repo != "" {
		if !strings.EqualFold(repo, cfg.GitHubRepo) {
			return false, fmt.Sprintf("entity's causely.ai/github-repo label (%s) does not match this instance's configured repo (%s)", repo, cfg.GitHubRepo)
		}
	}

	if len(cfg.ScopeNamespaces) > 0 {
		ns := strings.TrimSpace(payload.EntityNamespace)
		if ns == "" {
			return false, "SCOPE_NAMESPACES is configured but the root cause carried no entity namespace"
		}
		match := false
		for _, allowed := range cfg.ScopeNamespaces {
			if strings.EqualFold(ns, allowed) {
				match = true
				break
			}
		}
		if !match {
			return false, fmt.Sprintf("entity namespace (%s) is not in this instance's SCOPE_NAMESPACES (%s)", ns, strings.Join(cfg.ScopeNamespaces, ","))
		}
	}

	return true, ""
}
