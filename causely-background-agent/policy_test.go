package main

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadPolicyDefaultsToObserve(t *testing.T) {
	t.Parallel()
	p, err := loadPolicy(envFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.ActionMode != actionModeObserve || p.AllowKubeMutations || p.acting() {
		t.Fatalf("unexpected default policy: %+v", p)
	}
}

func TestLoadPolicyKubeMutationsRequireAct(t *testing.T) {
	t.Parallel()
	p, err := loadPolicy(envFrom(map[string]string{envAllowKubeMutation: "true"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.AllowKubeMutations {
		t.Fatal("kube mutations must stay off in observe mode")
	}
	p, err = loadPolicy(envFrom(map[string]string{envActionMode: "ACT", envAllowKubeMutation: "true",
		envGitHubRepo: "acme/shop", envScopeNamespaces: " shop, payments ,"}))
	if err != nil {
		t.Fatal(err)
	}
	if !p.acting() || !p.AllowKubeMutations || p.GitHubRepo != "acme/shop" || strings.Join(p.ScopeNamespaces, ",") != "shop,payments" {
		t.Fatalf("unexpected act policy: %+v", p)
	}
}

func TestLoadPolicyMaxTurnDuration(t *testing.T) {
	t.Parallel()
	p, _ := loadPolicy(envFrom(nil))
	if p.MaxTurnDuration != defaultMaxTurnDuration {
		t.Fatalf("default: %v", p.MaxTurnDuration)
	}
	p, err := loadPolicy(envFrom(map[string]string{envMaxTurnDuration: "0"}))
	if err != nil || p.MaxTurnDuration != 0 {
		t.Fatalf("0 should disable the limit: %v %v", p.MaxTurnDuration, err)
	}
	p, err = loadPolicy(envFrom(map[string]string{envMaxTurnDuration: "45m"}))
	if err != nil || p.MaxTurnDuration.Minutes() != 45 {
		t.Fatalf("45m: %v %v", p.MaxTurnDuration, err)
	}
}

func TestLoadPolicyRejectsInvalid(t *testing.T) {
	t.Parallel()
	for name, env := range map[string]map[string]string{
		"duration":  {envMaxTurnDuration: "-5m"},
		"mode":      {envActionMode: "yolo"},
		"bool":      {envAllowKubeMutation: "maybe"},
		"repo":      {envGitHubRepo: "https://github.com/acme/shop"},
		"namespace": {envScopeNamespaces: "Shop_Prod"},
	} {
		if _, err := loadPolicy(envFrom(env)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

// fnmatchcase reproduces Python's fnmatch.fnmatchcase for the subset of glob
// syntax denyPatterns uses (* and literal text). Hermes lower-cases both the
// command and the pattern before matching.
func fnmatchcase(name, pattern string) bool {
	var b strings.Builder
	b.WriteString("(?s)^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()).MatchString(name)
}

func denied(p policy, command string) bool {
	for _, pattern := range denyPatterns(p) {
		if fnmatchcase(strings.ToLower(command), strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

func TestDenyPatterns(t *testing.T) {
	t.Parallel()
	observe := policy{ActionMode: actionModeObserve}
	act := policy{ActionMode: actionModeAct}
	actMutate := policy{ActionMode: actionModeAct, AllowKubeMutations: true}

	cases := []struct {
		command string
		policy  policy
		deny    bool
	}{
		// Investigation stays possible in every mode.
		{"gh repo clone acme/shop repo -- --depth=50", observe, false},
		{"rg -n 'BRPop(' repo/", observe, false},
		{"gh pr list --repo acme/shop --state merged --limit 5", observe, false},
		{"gh api repos/acme/shop/commits?per_page=5", observe, false},
		{"kubectl get pods -n shop -o wide", observe, false},
		{"kubectl logs deploy/cart -n shop --since=30m", observe, false},
		{"kubectl describe secret db-creds -n shop", observe, false},
		{"kubectl get secrets -n shop", observe, false},
		{"git diff", observe, false},

		// Observe mode: no writes to GitHub.
		{"git push -u origin causely/fix-123", observe, true},
		{"gh pr create --title fix --body x", observe, true},
		{"gh api -X POST repos/acme/shop/pulls", observe, true},
		{"gh api repos/acme/shop/issues -f title=x", observe, true},
		{"gh pr comment 12 --body hi", observe, true},

		// Act mode: PRs allowed, default branch and merges are not.
		{"git push -u origin causely/fix-123", act, false},
		{"gh pr create --title fix --body x", act, false},
		{"git push origin main", act, true},
		{"git push origin HEAD:main", act, true},
		{"git push --force origin causely/fix-123", act, true},
		{"gh pr merge 12 --squash", act, true},

		// Secrets and credentials, in every mode.
		{"kubectl get secret db-creds -n shop -o yaml", actMutate, true},
		{"kubectl get secret db-creds -n shop -o jsonpath='{.data.password}'", actMutate, true},
		{"kubectl get --raw /api/v1/namespaces/shop/secrets", actMutate, true},
		{"kubectl exec -it cart-0 -- env", actMutate, true},
		{"echo $GH_TOKEN", actMutate, true},
		{"gh auth token", actMutate, true},
		{"kubectl config view --raw", actMutate, true},
		{"cat /var/run/secrets/kubernetes.io/serviceaccount/token", actMutate, true},

		// Cluster mutations: only restart/scale, only with the second gate.
		{"kubectl rollout restart deployment/cart -n shop", act, true},
		{"kubectl scale deployment/cart --replicas=3 -n shop", act, true},
		{"kubectl rollout restart deployment/cart -n shop", actMutate, false},
		{"kubectl -n shop scale deployment/cart --replicas=3", actMutate, false},
		{"kubectl delete pod cart-0 -n shop", actMutate, true},
		{"kubectl apply -f fix.yaml", actMutate, true},
		{"kubectl -n shop patch deploy cart -p '{}'", actMutate, true},
		{"kubectl rollout undo deployment/cart -n shop", actMutate, true},
		{"helm rollback cart 3", actMutate, true},
	}
	for _, tc := range cases {
		if got := denied(tc.policy, tc.command); got != tc.deny {
			t.Errorf("mode=%s mutations=%v %q: denied=%v, want %v", tc.policy.ActionMode, tc.policy.AllowKubeMutations, tc.command, got, tc.deny)
		}
	}
}

func TestBuildSoulReflectsPolicy(t *testing.T) {
	t.Parallel()
	observe := buildSoul("Investigate.", policy{ActionMode: actionModeObserve}, runtimeEnv{})
	for _, want := range []string{"Investigate.", "Action mode: OBSERVE", "kubectl: not configured", "Source repository: none configured", "Never read or print Secret values"} {
		if !strings.Contains(observe, want) {
			t.Fatalf("observe SOUL missing %q:\n%s", want, observe)
		}
	}
	act := buildSoul("", policy{ActionMode: actionModeAct, AllowKubeMutations: true, GitHubRepo: "acme/shop", ScopeNamespaces: []string{"shop"}},
		runtimeEnv{GitHubToken: true, Kubectl: true})
	for _, want := range []string{"Action mode: ACT", "kubectl rollout restart", "in namespaces shop", "gh repo clone acme/shop", "OUTCOME: out_of_scope"} {
		if !strings.Contains(act, want) {
			t.Fatalf("act SOUL missing %q:\n%s", want, act)
		}
	}
	if strings.HasPrefix(act, "\n") {
		t.Fatal("empty instruction should not leave leading blank lines")
	}
}

func TestLoadPolicySweepAndLedger(t *testing.T) {
	t.Parallel()
	p, err := loadPolicy(envFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxPerSweep != defaultMaxPerSweep || p.MaxSweepDuration != defaultMaxSweepDuration ||
		p.DedupWindow != defaultDedupWindow || p.Ledger != ledgerGitHubIssue || p.ledgerEnabled() {
		t.Fatalf("defaults: %+v (ledger needs a repo to be enabled)", p)
	}
	p, err = loadPolicy(envFrom(map[string]string{envGitHubRepo: "acme/shop", envMaxPerSweep: "5", envDedupWindow: "6h", envLedger: "OFF"}))
	if err != nil || p.MaxPerSweep != 5 || p.DedupWindow.Hours() != 6 || p.ledgerEnabled() {
		t.Fatalf("overrides: %+v %v", p, err)
	}
	for name, env := range map[string]map[string]string{
		"per sweep zero":  {envMaxPerSweep: "0"},
		"per sweep large": {envMaxPerSweep: "500"},
		"ledger":          {envLedger: "postgres"},
		"window":          {envDedupWindow: "yesterday"},
		"sweep duration":  {envMaxSweepDuration: "-1h"},
	} {
		if _, err := loadPolicy(envFrom(env)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestTurnLimitUsesSweepMarker(t *testing.T) {
	t.Parallel()
	p := policy{MaxTurnDuration: 20 * time.Minute, MaxSweepDuration: time.Hour}
	if got := p.turnLimit("Investigate Causely issue 123."); got != 20*time.Minute {
		t.Fatalf("single issue limit %s", got)
	}
	if got := p.turnLimit("\n  [causely-sweep]\nSweep active issues."); got != time.Hour {
		t.Fatalf("sweep limit %s", got)
	}
	if got := p.turnLimit("Please do not treat [causely-sweep] in the middle as a sweep"); got != 20*time.Minute {
		t.Fatalf("marker must lead the prompt, got %s", got)
	}
}

func TestDenyPatternsLedger(t *testing.T) {
	t.Parallel()
	ledger := policy{ActionMode: actionModeObserve, GitHubRepo: "acme/shop", Ledger: ledgerGitHubIssue}
	noLedger := policy{ActionMode: actionModeObserve, GitHubRepo: "acme/shop", Ledger: ledgerOff}
	act := policy{ActionMode: actionModeAct, GitHubRepo: "acme/shop", Ledger: ledgerGitHubIssue}
	lookup := `gh api "repos/acme/shop/issues?labels=causely-investigation&state=all&sort=updated&direction=desc&per_page=100" --jq '.[] | {number}'`
	create := "gh issue create --repo acme/shop --label causely-investigation --title 'Causely issue 42: x' --body-file /tmp/ledger.md"
	comment := "gh issue comment 7 --repo acme/shop --body-file /tmp/outcome.md"
	label := "gh label create causely-investigation --repo acme/shop --color 5319e7 --description 'Causely investigator ledger' 2>/dev/null || true"
	cases := []struct {
		command string
		policy  policy
		deny    bool
	}{
		{lookup, ledger, false},
		{lookup, noLedger, false},
		{create, ledger, false},
		{comment, ledger, false},
		{label, ledger, false},
		{create, noLedger, true},
		{comment, noLedger, true},
		{label, noLedger, true},
		{create, act, false},
		{"gh issue close 7 --repo acme/shop", ledger, true},
		{"gh issue edit 7 --repo acme/shop --title x", ledger, true},
		{"gh issue delete 7 --repo acme/shop --yes", act, true},
		{"gh label delete causely-investigation --repo acme/shop --yes", act, true},
		// The ledger does not reopen PR writes in observe mode.
		{"gh pr create --title fix --body x", ledger, true},
		{"git push -u origin causely/42-fix", ledger, true},
	}
	for _, tc := range cases {
		if got := denied(tc.policy, tc.command); got != tc.deny {
			t.Errorf("mode=%s ledger=%s %q: denied=%v, want %v", tc.policy.ActionMode, tc.policy.Ledger, tc.command, got, tc.deny)
		}
	}
}

func TestBuildSoulLedger(t *testing.T) {
	t.Parallel()
	pol := policy{ActionMode: actionModeObserve, GitHubRepo: "acme/shop", Ledger: ledgerGitHubIssue, DedupWindow: 24 * time.Hour, MaxPerSweep: 3}
	soul := buildSoul("", pol, runtimeEnv{GitHubToken: true})
	for _, want := range []string{"labeled `causely-investigation` in acme/shop", "causely-issue: <Causely ID>", "within the last 24h0m0s", "already_handled", "only GitHub writes you may make", "at most 3 issues"} {
		if !strings.Contains(soul, want) {
			t.Fatalf("SOUL missing %q:\n%s", want, soul)
		}
	}
	noToken := buildSoul("", pol, runtimeEnv{})
	if !strings.Contains(noToken, "Ledger: not available") {
		t.Fatalf("ledger without a token must be reported unavailable:\n%s", noToken)
	}
	scoped := buildSoul("", policy{ScopeNamespaces: []string{"shop"}, MaxPerSweep: 3}, runtimeEnv{})
	if !strings.Contains(scoped, "`causely.ai/namespace` label before calling any tool") || !strings.Contains(scoped, "namespace_names") {
		t.Fatalf("scope line should cover notification labels and sweeps:\n%s", scoped)
	}
}
