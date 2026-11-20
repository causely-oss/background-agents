package main

import (
	"fmt"
	"strings"
)

// buildSoul renders $HERMES_HOME/SOUL.md: the AgentTemplate system prompt
// followed by a section generated from Harness policy. The generated section
// comes last so it wins over anything the template says about mode, and it
// only states facts the harness also enforces (approvals.deny, credentials).
func buildSoul(instruction string, pol policy, env runtimeEnv) string {
	var b strings.Builder
	if s := strings.TrimSpace(instruction); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	b.WriteString("## Deployment policy (set by the Harness operator; takes precedence over the text above)\n\n")

	if pol.acting() {
		b.WriteString("- Action mode: ACT. When the outcome is a code fix, you may push a branch and open a pull request with `gh pr create`. Never push to the default branch and never merge.\n")
	} else {
		b.WriteString("- Action mode: OBSERVE. Investigate fully and report what you would do, but do not push branches, open or comment on pull requests, or call write endpoints. For a code fix, put the exact unified diff, PR title and PR body in your final answer instead. These commands are blocked by the harness.\n")
	}

	switch {
	case !env.Kubectl:
		b.WriteString("- kubectl: not configured in this deployment. Rely on Causely MCP config and topology tools for live state.\n")
	case pol.AllowKubeMutations:
		fmt.Fprintf(&b, "- kubectl: read access, plus `kubectl rollout restart` and `kubectl scale` for an immediate remediation%s. Every other cluster write is blocked. Only run a mutation after you have verified it addresses the cause, and say exactly what you ran.\n", namespaceClause(pol))
	default:
		b.WriteString("- kubectl: read-only (get, describe, logs, events, top). Cluster writes are blocked; recommend an immediate remediation as a command for a human to run.\n")
	}
	if env.Kubectl {
		b.WriteString("  kubectl output is the live cluster state at the moment you run it, which is stronger evidence than any cached or indexed view, including Causely's config tools. Prefer it when you need to confirm exactly what is deployed right now.\n")
	}

	switch {
	case pol.GitHubRepo != "" && env.GitHubToken:
		fmt.Fprintf(&b, "- Source repository: %s. Clone it with `gh repo clone %s repo -- --depth=50` into your workspace and search it locally with search_files or `rg`, which is much faster than walking directories.\n", pol.GitHubRepo, pol.GitHubRepo)
	case pol.GitHubRepo != "":
		fmt.Fprintf(&b, "- Source repository: %s, but no GitHub token is configured. If it is public you may clone it read-only; you cannot open a pull request.\n", pol.GitHubRepo)
	default:
		b.WriteString("- Source repository: none configured. You cannot verify or fix code; limit yourself to immediate remediation or no action needed.\n")
	}

	if len(pol.ScopeNamespaces) > 0 {
		fmt.Fprintf(&b, "- Scope: this deployment owns issues in namespaces %s. When the message carries the Causely notification JSON, check its `causely.ai/namespace` label before calling any tool; otherwise check the entity's namespace from the first Causely call. If it is outside these namespaces, stop with OUTCOME: out_of_scope. In a sweep, pass these as namespace_names to get_issues.\n", strings.Join(pol.ScopeNamespaces, ", "))
	}

	switch {
	case pol.ledgerEnabled() && env.GitHubToken:
		fmt.Fprintf(&b, "- Ledger: GitHub issues labeled `%s` in %s, one per Causely issue, each body containing `causely-issue: <Causely ID>`. Follow the ledger steps in the skill: check it before investigating, claim the issue, and record the outcome. Skip (OUTCOME: already_handled) an issue whose ledger entry was claimed or updated within the last %s, or that has an open fix PR, unless the message says `force`.%s\n",
			ledgerLabel, pol.GitHubRepo, pol.DedupWindow, observeLedgerNote(pol))
	default:
		b.WriteString("- Ledger: not available in this deployment, so you cannot tell whether an issue was already investigated. Investigate what you are given; in a sweep, still respect the per-sweep limit.\n")
	}
	fmt.Fprintf(&b, "- Sweep size: a sweep investigates at most %d issues; list the rest as not investigated.\n", pol.MaxPerSweep)
	b.WriteString("- Never read or print Secret values or credentials, including your own tokens. A Secret's key names are the most you may look at.\n")
	return b.String()
}

func observeLedgerNote(pol policy) string {
	if pol.acting() {
		return ""
	}
	return " In OBSERVE mode the ledger issue and its comments are the only GitHub writes you may make."
}

func namespaceClause(pol policy) string {
	if len(pol.ScopeNamespaces) == 0 {
		return ""
	}
	return " in namespaces " + strings.Join(pol.ScopeNamespaces, ", ")
}
