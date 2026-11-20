---
name: causely-investigation
description: Command recipes and the required report format for investigating Causely issues (one pushed issue, or a scheduled sweep) with Causely MCP, a GitHub-issue ledger, git, gh and kubectl, ending in a fix PR, an immediate remediation, or no action needed.
---

# Investigating Causely issues

Use this together with the system prompt, which sets the method (verify
before concluding, do not adopt Causely's remediation hint, prefer immediate
remediation over code) and the order of the per-issue pre-checks. This skill
covers the mechanics and the report. The "Deployment policy" section of your
system prompt says what this deployment allows: observe vs act, kubectl
access, the repository, the ledger, scope, and the sweep size. Commands that
policy forbids are blocked by the harness; do not try to work around a
block. Note it in your report and carry on.

Below, `REPO` is the repository from the deployment policy and `ID` is the
Causely issue ID.

## 0. Ledger (when the deployment policy has one)

The ledger is one GitHub issue per Causely issue in `REPO`, labeled
`causely-investigation`, whose body contains the line `causely-issue: ID`.
Comments on it record each claim and each outcome. It is how separate,
short-lived runs (mediator pushes, sweeps, people) avoid investigating the
same issue twice.

**Look up.** Use the REST list endpoint, not search (search lags new
issues by up to a minute). One call returns the recent ledger, so a sweep can
check every candidate at once:

```bash
gh api "repos/REPO/issues?labels=causely-investigation&state=all&sort=updated&direction=desc&per_page=100" \
  --jq '.[] | {number, state, updated_at, html_url, id_line: (.body | capture("causely-issue: (?<id>\\S+)").id)}'
gh pr list --repo REPO --state open --json number,url,headRefName,body \
  --jq '.[] | select(.headRefName | startswith("causely/")) | {number, url, headRefName}'
```

Skip the issue as `already_handled` when its ledger entry's `updated_at` is
within the dedup window, or when an open PR's branch starts with
`causely/ID` or its body contains `causely-issue: ID`. A message containing
the word `force` overrides this. Do not comment when you skip.

**Claim** before the deep investigation (right after the "still active"
check), so a parallel run sees it:

```bash
gh label create causely-investigation --repo REPO --color 5319e7 \
  --description "Causely investigator ledger" 2>/dev/null || true
# only if no ledger issue exists yet:
printf 'causely-issue: %s\n\nEntity: %s\nCausely: %s\n' "ID" "<entity> (<namespace>)" "<link>" > /tmp/ledger.md
gh issue create --repo REPO --label causely-investigation \
  --title "Causely issue ID: <issue name>" --body-file /tmp/ledger.md
gh issue comment <number> --repo REPO \
  --body "claimed $(date -u +%Y-%m-%dT%H:%M:%SZ) by <single-issue|sweep> run, mode <observe|act>"
```

**Record** the outcome when you finish the issue: comment the issue's
OUTCOME block (and, for `fix_pr`, the PR URL) with
`gh issue comment <number> --repo REPO --body-file /tmp/outcome.md`. Never
close, edit or delete ledger issues; people do that.

## 1. Read the issue from Causely

Start from the issue ID in the request.

- `mcp__causely__get_issue_details`: the issue, its symptoms and evidence.
  Check its `state` first: if it is no longer active, stop with
  `no_action_needed` ("resolved before investigation").
- `mcp__causely__get_diagnosis_details` / `get_diagnosis_observable_signals`:
  why Causely thinks this is the cause.
- `mcp__causely__get_topology`, `get_entity_health`, `get_service_summary`:
  blast radius and what depends on what.
- `mcp__causely__get_logs`, `get_events`, `get_metrics`: raw evidence.
- `mcp__causely__get_config`: indexed config, which may lag the cluster.

If a scope is configured and the entity's namespace is outside it, stop and
report `out_of_scope`.

## 2. Check live cluster state (when kubectl is configured)

Read-only commands that work in every mode:

```bash
kubectl get deploy,sts,ds,pods -n <ns> -o wide
kubectl describe deploy/<name> -n <ns>
kubectl get deploy/<name> -n <ns> -o yaml       # live spec, env, image, resources
kubectl logs deploy/<name> -n <ns> --since=1h --tail=300
kubectl logs <pod> -n <ns> --previous --tail=300 # last crash
kubectl get events -n <ns> --sort-by=.lastTimestamp | tail -40
kubectl rollout history deploy/<name> -n <ns>
kubectl top pods -n <ns>
```

Never request a Secret's contents (`-o yaml/json/jsonpath` on a Secret,
`--raw`, `exec`); those are blocked. `kubectl describe secret` shows key
names and sizes, if RBAC lets you see Secrets at all.

## 3. Read the code

Clone shallowly into your workspace and search locally; never walk a large
repository directory by directory.

```bash
gh repo clone <owner/repo> repo -- --depth=50
cd repo
rg -n --hidden -g '!.git' '<exact error string or symbol>'
rg -n '\.BRPop\(' --type go          # the real call site, not the word
git log --oneline -20 -- <path>       # recent changes near the suspect code
gh pr list --state merged --limit 10 --search '<keyword>'
```

Use `search_files` / `read_file` for targeted reads once you know the file.

## 4. Finish

### Code fix (`fix_pr`)

1. `git switch -c causely/<issue-id>-<short-slug>`
2. Make the smallest change that fixes the cause. Run the project's own
   formatter or a quick compile/test if it is cheap and available.
3. `git diff --stat` must be non-empty. If the repository already contains
   what your fix would produce, this is not a code fix: re-classify it as
   config drift (`remediation`) or `no_action_needed`.
4. In ACT mode:
   ```bash
   git -c user.name='Causely Investigator' -c user.email='causely-investigator@users.noreply.github.com' \
     commit -am '<title>'
   git push -u origin HEAD
   gh pr create --title '<title>' --body-file /tmp/pr-body.md
   ```
   The PR body starts with the line `causely-issue: ID` (the ledger keys on
   it), then the Causely issue ID and diagnosis, the evidence you verified
   (with what you checked and how), the root cause, the change, and the
   risk. Put the PR URL in your report and in the ledger.
5. In OBSERVE mode: do not commit or push. Put `git diff` output, the PR
   title and the PR body in your report.

### Immediate remediation (`remediation`)

Give the exact command(s) and why they address the verified cause. Run a
restart or scale yourself only when the deployment policy says kubectl
mutations are enabled, then confirm the result (`kubectl rollout status`,
fresh logs) and say exactly what you ran. Otherwise the commands are a
recommendation for a human.

### No action needed (`no_action_needed`)

Say what you verified that shows nothing needs to change: for example the
exception is caught and handled at file:line, or the live state already
matches what a fix would set.

### Skipped (`already_handled`, `out_of_scope`)

One or two sentences: the ledger entry or PR URL, or the namespace and the
scope.

## 5. Sweep mode

For a `[causely-sweep]` request:

1. `mcp__causely__get_issues` with `active_only: true`, the scope namespaces
   as `namespace_names` (if the policy has any), and any severity filter the
   sweep prompt asks for.
2. Run the ledger lookup once and drop issues that are `already_handled`
   or `out_of_scope`.
3. Order the rest by severity (Critical, High, Medium, Low), then oldest
   `started_at` first, and take at most the per-sweep limit.
4. Run the full per-issue procedure on each in turn (still-active check,
   claim, investigate, finish, record). Start each issue's code work in a
   fresh directory (`repo-ID`) so one issue's changes cannot leak into the
   next issue's PR.
5. Report as below.

## Report format

### Single issue

Your final message must start with this block so callers can parse it, then
the explanation in plain prose:

```
OUTCOME: fix_pr | remediation | no_action_needed | already_handled | out_of_scope | blocked
ISSUE: <Causely issue ID>
ENTITY: <entity name> (<namespace>)
ACTION_TAKEN: <PR URL | command you ran | ledger issue URL | none>
SUMMARY: <one or two sentences>
```

### Sweep

One OUTCOME block (same fields) per issue you investigated or skipped,
separated by `---` lines, each followed by its short explanation; then a
final block:

```
SWEEP_SUMMARY: listed=<n> investigated=<n> skipped=<n> not_reached=<n>
NOT_REACHED: <comma-separated Causely IDs left for the next sweep, or none>
```

Then, for each investigated issue:

- **Evidence**: each fact you relied on and where it came from (tool and
  arguments, file:line, command). Mark anything you inferred but could not
  confirm as unconfirmed.
- **Root cause** (or why there is none).
- **Recommended action**: the diff, PR title and body, or the commands.
- **What Causely got right or wrong**, briefly, when your conclusion differs
  from its diagnosis.
