package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Harness environment variables that carry deployment policy. They are set on
// the Harness (spec.env), not the AgentTemplate, so a prompt edit can never
// turn on act mode: the harness turns them into both Hermes approvals.deny
// rules (deterministic) and a generated SOUL.md section (advisory).
const (
	envActionMode        = "CAUSELY_ACTION_MODE"          // observe (default) | act
	envAllowKubeMutation = "CAUSELY_ALLOW_KUBE_MUTATIONS" // true enables restart/scale in act mode
	envGitHubRepo        = "CAUSELY_GITHUB_REPO"          // owner/name the agent may read and fix
	envScopeNamespaces   = "CAUSELY_SCOPE_NAMESPACES"     // comma-separated; empty = no restriction
	envMaxTurnDuration   = "CAUSELY_MAX_TURN_DURATION"    // Go duration; 0 disables; default 20m
	envMaxSweepDuration  = "CAUSELY_MAX_SWEEP_DURATION"   // Go duration for a sweep turn; 0 disables; default 60m
	envMaxPerSweep       = "CAUSELY_MAX_INVESTIGATIONS_PER_SWEEP"
	envLedger            = "CAUSELY_LEDGER"          // github-issue (default) | off
	envDedupWindow       = "CAUSELY_DEDUP_WINDOW"    // Go duration; default 24h
	envKubeconfigData    = "KUBECONFIG_DATA"         // optional kubeconfig contents (standalone runs)
	envKubeAPIServer     = "CAUSELY_KUBE_API_SERVER" // API server URL; the token is injected at egress
	envGitHubToken       = "GH_TOKEN"                // read by gh and git (via gh credential helper)
	actionModeObserve    = "observe"
	actionModeAct        = "act"
	ledgerGitHubIssue    = "github-issue"
	ledgerOff            = "off"
	// ledgerLabel marks the GitHub issues that record investigations.
	ledgerLabel = "causely-investigation"
)

type policy struct {
	ActionMode string
	// AllowKubeMutations is only ever true together with act mode.
	AllowKubeMutations bool
	GitHubRepo         string
	ScopeNamespaces    []string
	// MaxTurnDuration cancels a Hermes turn that runs longer than this.
	MaxTurnDuration time.Duration
	// MaxSweepDuration replaces MaxTurnDuration for a sweep turn (a prompt
	// that starts with sweepMarker), which investigates several issues.
	MaxSweepDuration time.Duration
	// MaxPerSweep caps how many issues one sweep investigates.
	MaxPerSweep int
	// Ledger is ledgerGitHubIssue or ledgerOff. The ledger is a GitHub issue
	// per Causely issue in GitHubRepo, labeled ledgerLabel; it is how
	// ephemeral Actors know an issue was already investigated.
	Ledger      string
	DedupWindow time.Duration
}

const (
	defaultMaxTurnDuration  = 20 * time.Minute
	defaultMaxSweepDuration = 60 * time.Minute
	defaultMaxPerSweep      = 3
	maxPerSweepLimit        = 20
	defaultDedupWindow      = 24 * time.Hour
)

// sweepMarker starts the prompt of a scheduled sweep (see
// deploy/kagent/scheduled-sweep.json). The system prompt keys sweep mode off
// it, and the runner uses it to apply MaxSweepDuration instead of
// MaxTurnDuration.
const sweepMarker = "[causely-sweep]"

func isSweepPrompt(prompt string) bool {
	return strings.HasPrefix(strings.TrimSpace(prompt), sweepMarker)
}

// turnLimit is the wall-clock limit for one turn with this prompt.
func (p policy) turnLimit(prompt string) time.Duration {
	if isSweepPrompt(prompt) {
		return p.MaxSweepDuration
	}
	return p.MaxTurnDuration
}

// ledgerEnabled reports whether the agent can actually use the ledger: it
// needs a repository, and the caller must also have a GitHub token.
func (p policy) ledgerEnabled() bool { return p.Ledger == ledgerGitHubIssue && p.GitHubRepo != "" }

func parseDuration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration like 20m, got %q", name, raw)
	}
	return d, nil
}

func (p policy) acting() bool { return p.ActionMode == actionModeAct }

var (
	repoPattern      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	namespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

func loadPolicy(getenv func(string) string) (policy, error) {
	p := policy{ActionMode: strings.ToLower(strings.TrimSpace(getenv(envActionMode)))}
	switch p.ActionMode {
	case "":
		p.ActionMode = actionModeObserve
	case actionModeObserve, actionModeAct:
	default:
		return policy{}, fmt.Errorf("%s must be %q or %q, got %q", envActionMode, actionModeObserve, actionModeAct, p.ActionMode)
	}
	if raw := strings.TrimSpace(getenv(envAllowKubeMutation)); raw != "" {
		allow, err := strconv.ParseBool(raw)
		if err != nil {
			return policy{}, fmt.Errorf("%s: %w", envAllowKubeMutation, err)
		}
		// Mirrors the old service: a second gate on top of act mode, so
		// flipping act on for PRs does not also authorize cluster writes.
		p.AllowKubeMutations = allow && p.acting()
	}
	p.GitHubRepo = strings.TrimSpace(getenv(envGitHubRepo))
	if p.GitHubRepo != "" && !repoPattern.MatchString(p.GitHubRepo) {
		return policy{}, fmt.Errorf("%s must look like owner/name, got %q", envGitHubRepo, p.GitHubRepo)
	}
	for _, ns := range strings.Split(getenv(envScopeNamespaces), ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if !namespacePattern.MatchString(ns) {
			return policy{}, fmt.Errorf("%s contains invalid namespace %q", envScopeNamespaces, ns)
		}
		p.ScopeNamespaces = append(p.ScopeNamespaces, ns)
	}
	var err error
	if p.MaxTurnDuration, err = parseDuration(getenv, envMaxTurnDuration, defaultMaxTurnDuration); err != nil {
		return policy{}, err
	}
	if p.MaxSweepDuration, err = parseDuration(getenv, envMaxSweepDuration, defaultMaxSweepDuration); err != nil {
		return policy{}, err
	}
	if p.DedupWindow, err = parseDuration(getenv, envDedupWindow, defaultDedupWindow); err != nil {
		return policy{}, err
	}
	p.MaxPerSweep = defaultMaxPerSweep
	if raw := strings.TrimSpace(getenv(envMaxPerSweep)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxPerSweepLimit {
			return policy{}, fmt.Errorf("%s must be an integer from 1 to %d, got %q", envMaxPerSweep, maxPerSweepLimit, raw)
		}
		p.MaxPerSweep = n
	}
	p.Ledger = strings.ToLower(strings.TrimSpace(getenv(envLedger)))
	switch p.Ledger {
	case "":
		p.Ledger = ledgerGitHubIssue
	case ledgerGitHubIssue, ledgerOff:
	default:
		return policy{}, fmt.Errorf("%s must be %q or %q, got %q", envLedger, ledgerGitHubIssue, ledgerOff, p.Ledger)
	}
	return p, nil
}

// denyPatterns returns Hermes approvals.deny globs. Hermes matches them
// case-insensitively with fnmatch against normalized/deobfuscated variants of
// every terminal command, before any approval-bypass mode. They are defense
// in depth: Hermes' execute_code tool can spawn processes without going
// through terminal(), so the real boundary is what the credentials allow
// (RBAC on the kubeconfig, scopes on GH_TOKEN). See README "Safety model".
func denyPatterns(p policy) []string {
	deny := []string{
		// Never print credentials. The adapter keeps them out of the terminal
		// environment (see prepareProcessEnvironment); these block the obvious
		// ways to go looking for them anyway.
		"*gh_token*",
		"*anthropic_api_key*",
		"*openai_api_key*",
		"*kubeconfig_data*",
		"*kagent_credential*",
		"*causely-secrets*",
		"*/proc/*/environ*",
		"*gh* auth token*",
		"*git-credential*",
		"*credential fill*",
		"*--show-token*",
		"*kubectl* config view*--raw*",
		"*kubectl* create token*",
		"*/var/run/secrets/*",
		// Never read Secret values. Listing names (kubectl get secrets) and
		// key names with sizes (kubectl describe secret) stay allowed if RBAC
		// permits; any structured output of a Secret is blocked.
		"*kubectl* secret*-o*",
		"*kubectl* secret*--output*",
		"*kubectl* secret*--template*",
		"*kubectl*--raw*",
		"*kubectl* exec*",
		"*kubectl* cp *",
		"*kubectl* attach*",
		"*kubectl* debug*",
		"*kubectl* port-forward*",
		"*kubectl* proxy*",
		// Cluster writes that are never part of a remediation here.
		"*kubectl* apply*",
		"*kubectl* create*",
		"*kubectl* delete*",
		"*kubectl* patch*",
		"*kubectl* edit*",
		"*kubectl* replace*",
		"*kubectl* set *",
		"*kubectl* label *",
		"*kubectl* annotate *",
		"*kubectl* drain*",
		"*kubectl* cordon*",
		"*kubectl* uncordon*",
		"*kubectl* taint*",
		"*kubectl* autoscale*",
		"*kubectl* expose*",
		"*kubectl* run *",
		"*kubectl* rollout undo*",
		"*kubectl* rollout pause*",
		"*kubectl* rollout resume*",
		"*helm* install*",
		"*helm* upgrade*",
		"*helm* uninstall*",
		"*helm* rollback*",
		// GitHub actions that are never the agent's call, even in act mode.
		"*git* push*--force*",
		"*git* push* -f*",
		"*git* push*:main*",
		"*git* push* main",
		"*git* push* main *",
		"*git* push*:master*",
		"*git* push* master",
		"*git* push* master *",
		"*gh* pr merge*",
		"*gh* pr review*",
		"*gh* repo delete*",
		"*gh* repo edit*",
		"*gh* secret*",
		"*gh* variable*",
		"*gh* workflow run*",
	}
	if !p.AllowKubeMutations {
		deny = append(deny,
			"*kubectl* scale*",
			"*kubectl* rollout restart*",
		)
	}
	// The ledger only ever creates issues and comments on them.
	deny = append(deny,
		"*gh* issue delete*",
		"*gh* issue transfer*",
		"*gh* issue lock*",
		"*gh* label delete*",
	)
	if !p.acting() {
		// Observe mode: report only. No branch pushes, no PRs, no write
		// calls through the raw API. The one write allowed is the ledger
		// (creating and commenting on causely-investigation issues), and
		// only when it is enabled; the GitHub token then needs Issues write
		// but still no Contents or Pull requests write.
		deny = append(deny,
			"*git* push*",
			"*gh* pr create*",
			"*gh* pr edit*",
			"*gh* pr comment*",
			"*gh* pr close*",
			"*gh* pr ready*",
			"*gh* issue edit*",
			"*gh* issue close*",
			"*gh* release*",
			"*gh* repo create*",
			"*gh* repo fork*",
			"*gh* api* -x*",
			"*gh* api*--method*",
			"*gh* api* -f*",
			"*gh* api*--field*",
			"*gh* api*--raw-field*",
			"*gh* api*--input*",
		)
		if !p.ledgerEnabled() {
			deny = append(deny,
				"*gh* issue create*",
				"*gh* issue comment*",
				"*gh* label create*",
			)
		}
	}
	return deny
}
