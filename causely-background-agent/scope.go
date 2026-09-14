package main

import (
	"fmt"
	"strings"
)

// inScope reports whether this remediator instance may act on the triggering
// root cause. Mediator-side routing is best-effort, not a guarantee, so a
// remediator must not open a PR against a repo it wasn't configured for.
//
// Independent, non-exclusive signals checked, in order:
//  1. GitHubRepoLabel — if set, must match this instance's configured GitHubRepo.
//  2. ScopeNamespaces — if configured, the entity's namespace must be in the list.
//  3. AllowedSeverities — if configured, payload.Severity must be in the list.
//     Applies uniformly to every trigger source (webhook, poll, Slack). Added
//     after finding live that Causely's own root-cause severity can flicker
//     between a baseline and an elevated value as a chronic condition merely
//     toggles active/inactive, which — combined with poll's watermark falling
//     back to severity when there's no updated_at — produced duplicate paid
//     investigations of the same already-known Low/Medium-severity flapping
//     issue. Restricting to High/Critical filters that noise out entirely,
//     independent of whatever poll's watermark does.
//
// If none of the configured signals reject it, the trigger is accepted.
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

	if len(cfg.AllowedSeverities) > 0 {
		sev := strings.TrimSpace(payload.Severity)
		match := false
		for _, allowed := range cfg.AllowedSeverities {
			if strings.EqualFold(sev, allowed) {
				match = true
				break
			}
		}
		if !match {
			return false, fmt.Sprintf("severity (%s) is not in this instance's allowed_severities (%s)", sev, strings.Join(cfg.AllowedSeverities, ","))
		}
	}

	return true, ""
}
