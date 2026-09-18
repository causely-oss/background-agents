# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
"""
eval_scenarios.py — Causely vs. baseline, head to head, across complex-demo's
scenarios 9/10/11 (billing missing timeout / recommendation OOM / pricing N+1).

For each scenario, in order:
  1. Deploy the regression into the live cluster (scenario-01), mirroring that
     scenario's own README exactly (same git checkouts, same deploy.sh/kubectl
     commands you'd run by hand).
  2. Fork a throwaway branch per agent off the scenario's neutral branch tip —
     NOT the shared neutral branch itself, so two agents committing fixes at
     the same time can't stomp on each other or leave the reusable branch
     mutated for the next run.
  3. Run both agents (Causely-enabled, baseline) CONCURRENTLY against the same
     live incident: one `user.define_outcome` event each (generic prompt +
     scenario-specific grading rubric), streamed until the platform's own
     grader reaches a terminal verdict.
  4. Record per agent: verdict, grader explanation, PR opened (if any),
     duration/active seconds, token usage.
  5. Restore the cluster to baseline before moving to the next scenario.

Ends with a comparison summary (printed + written to a JSON report) and does
NOT touch main or deploy either agent's fix — the two agents' PRs are left
open for you to read/merge/close by hand.

Requires .env already set up per this repo's README: ANTHROPIC_API_KEY,
K8S_MCP_URL, GRAFANA_MCP_URL(+GRAFANA_TOKEN), CAUSELY_* (client-credentials),
GITHUB_REPO_URL, GITHUB_TOKEN (needs push access — checked at startup).

Usage:
    .venv/bin/python3 eval_scenarios.py
    .venv/bin/python3 eval_scenarios.py --scenarios 10            # just one, for a dry run
    .venv/bin/python3 eval_scenarios.py --timeout-min 45 --settle-sec 90

Note on --timeout-min: a real investigate-diagnose-fix-PR cycle isn't quick —
a scenario-10 dry run measured ~18 min (baseline agent) and ~23 min (Causely
agent) end to end. Don't go below ~25 even for "just testing the pipeline";
the remote session keeps running (and billing) after this script's own
timeout gives up on it, so a too-short value doesn't save you time, it just
means you never see the real result and the script has to interrupt a
session that might have finished in another minute or two.

Expect, per scenario: deploy (~10s-3min) + settle (600s, for Causely's ~5min
symptom-activation delay) + the slower agent's runtime (both agents run
concurrently, so this isn't additive between them; ~8-15min observed) +
restore (~1min) + cooldown before the next scenario (600s default, for
Causely's ~5min symptom-DEACTIVATION delay — otherwise the just-resolved
issue can still look "recent" when the next scenario's agents call
get_issues; a 300s cooldown wasn't enough in one dry run, where a
still-forming recommendation-service issue from the prior scenario cost
the next scenario's Causely agent a full extra investigate-fix-revert
cycle). That's roughly ~25-30min/scenario typically, so ~75-90min for all
three scenarios back to back — more if an agent needs multiple
outcome-revision iterations or hits --timeout-min.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

import anthropic
import httpx
from dotenv import load_dotenv

load_dotenv()
client = anthropic.Anthropic()

# ── Configuration ────────────────────────────────────────────────────────
REPO_DIR = Path(os.environ.get(
    "COMPLEX_DEMO_REPO",
    Path.home() / "Documents/GitHub/development-environment/complex-demo",
))
NAMESPACE = os.environ.get("K8S_NAMESPACE", "scenario-01")

K8S_MCP_URL = os.environ["K8S_MCP_URL"]
GRAFANA_MCP_URL = os.environ.get("GRAFANA_MCP_URL", "")
GRAFANA_TOKEN = os.environ.get("GRAFANA_TOKEN", "")
CAUSELY_MCP_URL = os.environ.get("CAUSELY_MCP_URL", "")
MODEL = os.environ.get("ANTHROPIC_MODEL", "claude-opus-4-7")

GITHUB_REPO_URL = os.environ["GITHUB_REPO_URL"].removesuffix(".git")
GITHUB_OWNER_REPO = GITHUB_REPO_URL.split("github.com/", 1)[-1]
GITHUB_TOKEN = os.environ["GITHUB_TOKEN"]

RUN_ID = os.environ.get("EVAL_RUN_ID") or datetime.now().strftime("%Y%m%d-%H%M%S")
REPORT_PATH = Path(f"eval_report_{RUN_ID}.json")

# The agent is never told which scenario is live or what the bug "is" — it
# only gets pointed at the affected service, same as a real on-call handoff
# naming the service that paged. Only the grading rubric (below, per
# scenario) says anything about the actual defect, and that's consumed by
# the platform's grader, not handed to the agent as an instruction.
def generic_prompt(service: str) -> str:
    return f"""On-call handoff: something appears to be degrading the {service} in \
the `{NAMESPACE}` namespace right now. Investigate using the tools \
available to you, identify the root cause, and implement a fix. Report a \
short summary of the root cause and the fix when you're done."""

_CAUSELY_FIRST = f"""

You also have Causely, a causal intelligence layer that already computes \
root cause for this system. Call `get_issues(namespace_names=["{NAMESPACE}"])` \
FIRST — before any k8s or Grafana tool call, and before any other Causely \
tool. ALWAYS pass that namespace filter explicitly: the Causely backend \
serves many unrelated tenants/environments besides this one, and an \
unfiltered `get_issues()` call returns issues from all of them mixed \
together, which makes finding the real one much harder, not easier.

This namespace accumulates old, unrelated issues that never get closed — \
`get_issues` will typically return several ACTIVE issues at once, most of \
them stale leftovers from unrelated incidents hours or days old. Do not \
just take the first result.

First, check recency using ONLY the fields `get_issues` already gave you \
— no extra call yet. Compare each issue's top-level `started_at` to the \
current time. If the single most-recent `started_at` across ALL returned \
issues is already more than ~30 minutes old, stop here: treat this as no \
genuinely active issue right now, and skip straight to the \
`get_service_summary` fallback below instead of drilling into any of \
them — do not call `get_issue_details` at all in this case, it will \
waste time confirming issues that are all stale.

Only if at least one issue's `started_at` is recent (within ~30 minutes): \
call `get_issue_details` ONCE, on that single most-recent issue, to \
confirm it before committing: look at its `symptoms` list for one with a \
`started_at` that is ALSO recent and no `ended_at`. Don't just trust a \
bare `active: true` flag by itself — a symptom can be stuck showing \
`active: true` forever on a pod/entity that no longer exists, so the \
recency of `started_at` is what matters, not the flag. If confirmed, that \
issue's primary_diagnosis is your root cause. If it does NOT hold up and \
another issue also had a recent top-level `started_at`, check that one \
next the same way.

If `get_issues` doesn't pan out — nothing recent, or the recent \
candidate(s) didn't confirm — before falling back to raw k8s/Grafana, \
call `get_service_summary` ONCE for the specific service named in your \
on-call handoff. It aggregates that service's active symptoms, \
diagnoses, SLOs, resource metrics, dependency health, and recent \
events/errors in a single call, and can surface something `get_issues` \
missed (Causely can under-report a recurring issue's recency at the \
issue level while the service itself shows genuinely active symptoms). \
Only fall back to raw k8s/Grafana tools if that also comes up empty or \
unhelpful.

No Causely tool is off-limits — use whichever ones genuinely help. But \
once an issue is confirmed (via `get_issues`/`get_issue_details`, or via \
that `get_service_summary` fallback), you already have your root cause: \
go directly from its primary_diagnosis/diagnosis to reading and fixing \
the implicated source or config, rather than spending more calls \
(Causely or k8s/Grafana) re-deriving or re-verifying what it already \
told you."""

_BASE_SYSTEM_PROMPT = f"""You are an SRE agent investigating a live Kubernetes cluster. You have \
tools for:
- Kubernetes: list and inspect namespaces, pods, deployments, services, \
events, and pod logs.
- Grafana (if available): query Prometheus metrics, Loki logs, and Tempo \
traces.
- A git checkout of this project's repository in your sandbox filesystem, \
with shell and file-editing tools. Your git credential can push commits but \
cannot authenticate opening a pull request — don't try (via `gh pr create`, \
the GitHub API, or anything else); it will fail and waste your time.

Investigate the ACTUAL cluster with these tools — you have no pre-loaded \
data, do not rely on prior assumptions. Your scope is fixed to the \
`{NAMESPACE}` namespace: never list, inspect, or query resources, events, \
metrics, or logs from any other namespace.

Approach:
1. Identify the affected workload within `{NAMESPACE}`.
2. Check workload state: restarts, CrashLoopBackOff, pending, OOMKills, \
readiness.
3. Check recent Kubernetes events for `{NAMESPACE}`.
4. Pull logs for the failing workload AND its in-namespace dependencies, \
filtering for errors.
5. Correlate across workloads: a symptom in one service is often caused by \
a dependency. Distinguish the failing service from the underlying cause.
6. Once you're confident in the root cause, read the relevant source/config \
in your repository checkout and write the fix. Commit it, then run \
`git push origin HEAD` to push to the branch that's already checked out — \
do not create, rename, or switch to a different branch. Do not open a pull \
request yourself.

Every factual claim must come from a tool call — never invent numbers or \
guess. State which observation supports each conclusion."""


# ── Per-scenario grading rubrics ────────────────────────────────────────
# Only used by the platform's automatic grader (via user.define_outcome) —
# not shown to the agent as an instruction. Each says what a correct fix
# looks like AND flags the traps (a plausible-but-wrong fix in the adjacent
# service, or a config toggle instead of a code fix).
RUBRICS = {
    "09": """A correct fix restores a bounded request timeout (a few seconds, \
consistent with the rest of the fleet's client timeouts) on billing-service's \
outbound HTTP client — the `otelHTTPClient` in \
environment/services/billing-service/main.go used for its downstream calls \
to payment-adapter, tax-service, and fraud-detection. The regression removed \
the client's `Timeout` field when it was reconstructed for connection \
pooling. Grade the resulting behavior, not the exact diff shape: keeping the \
connection pooling while adding the timeout back is fine, and so is \
reverting to a simpler client construction as long as a bounded timeout \
ends up in place.

INCORRECT / does not satisfy: changing payment-adapter's code or config \
(payment-adapter is the environmental trigger, not the root cause); adding a \
fault-injection admin toggle; scaling replicas or resources instead of \
fixing the client; any fix that does not touch billing-service's \
otelHTTPClient definition or leaves it without a bounded timeout.""",

    "10": """A correct fix raises recommendation-service's memory `requests`/\
`limits` in k8s/03-app.yaml to a value that actually fits its real working \
set — comfortably above its observed peak (which sits around 10-13MB; the \
regression set requests/limits to 8Mi/10Mi). It does not need to match any \
particular number exactly, but must not leave the limit anywhere near that \
~10-13MB region.

INCORRECT / does not satisfy: touching recommendation-service's Go source \
code (this is a pure manifest/resource-sizing regression, not an \
application bug); changing an unrelated service's resources instead of \
recommendation-service's; merely adding replicas or an HPA change without \
fixing the memory limit itself; restarting the pod or deployment as a \
workaround.""",

    "11": """A correct fix restores pricing-service's calculateHandler \
(environment/services/pricing-service/main.go) to compute one aggregate \
`baseTotal`/subtotal across all items in the cart and make a SINGLE call to \
discount-service per request — instead of looping over each line item and \
calling discount-service once per item.

INCORRECT / does not satisfy: changing discount-service instead of \
pricing-service (the call pattern is wrong, not the callee); adding \
caching, batching, or rate-limiting as a band-aid without removing the \
per-item loop; any fix that leaves discount-service's call volume scaling \
with cart size instead of with pricing-service's own request rate.""",
}


# ── Shell helpers ────────────────────────────────────────────────────────
def sh(cmd: list[str], cwd: Path = REPO_DIR, check: bool = True) -> str:
    print(f"    $ {' '.join(cmd)}")
    result = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
    if result.stdout.strip():
        print("      " + result.stdout.strip().replace("\n", "\n      "))
    if check and result.returncode != 0:
        print("      " + result.stderr.strip().replace("\n", "\n      "))
        raise RuntimeError(f"command failed ({result.returncode}): {' '.join(cmd)}")
    return result.stdout


def kubectl(*args: str, check: bool = True) -> str:
    return sh(["kubectl", "-n", NAMESPACE, *args], check=check)


# ── Scenario definitions ────────────────────────────────────────────────
# deploy()/restore() are exactly the commands each scenario's own README
# documents for an operator — nothing new invented here.
@dataclass
class Scenario:
    key: str
    name: str
    neutral_branch: str  # forked for each agent's throwaway eval branch
    deploy: callable
    restore: callable
    rubric: str
    # Named in the agent's on-call prompt. Deliberately NOT the service whose
    # code/config actually has the bug — that would hand the agent the
    # answer. Picked from that service's real upstream dependents (via
    # get_topology mode=dependents against live Causely topology) at least 2
    # hops away, so the agent has to trace the blast radius back to the
    # actual root cause instead of being told where to look.
    affected_service: str
    # Causely has a default ~5 minute symptom-activation delay before a
    # sustained symptom is promoted to an active diagnosis — 90s was nowhere
    # near enough. Launching the agents before that window closes means
    # get_diagnoses can come back empty/weak even though the regression is
    # genuinely live, forcing even a well-behaved Causely-first agent into a
    # full manual k8s/Grafana investigation for reasons that have nothing to
    # do with how it follows instructions. 10 min gives comfortable margin
    # above the ~5 min default rather than cutting it close.
    settle_sec: int = 600


def _deploy_09():
    sh(["git", "checkout", "scenario-09-billing-timeout-bug"])
    sh(["git", "checkout", "chore/billing-http-client-pooling", "--",
        "environment/services/billing-service"])
    sh(["bash", "scenarios/09-billing-missing-timeout/deploy.sh"])
    # Long-lived trigger: the default auto-restores after 150s, too short to
    # survive a real multi-minute agent investigation. Keep it live for the
    # duration of this scenario's run; restore() below cuts it short.
    env = {**os.environ, "LATENCY_MS": "3000", "DURATION_SECONDS": "3600"}
    subprocess.run(["bash", "scenarios/09-billing-missing-timeout/trigger.sh"],
                    cwd=REPO_DIR, env=env, check=True)


def _restore_09():
    sh(["bash", "scenarios/09-billing-missing-timeout/restore.sh"], check=False)
    kubectl("set", "image", "deploy/billing-service",
            "billing-service=byemini2/billing-service:v4")
    kubectl("rollout", "status", "deploy/billing-service", "--timeout=120s")


def _deploy_10():
    sh(["git", "checkout", "scenario-10-recommendation-oom-bug"])
    kubectl("apply", "-f", str(REPO_DIR / "k8s/03-app.yaml"))
    kubectl("rollout", "status", "deploy/recommendation-service", "--timeout=120s")


def _restore_10():
    sh(["git", "checkout", "main"])
    kubectl("apply", "-f", str(REPO_DIR / "k8s/03-app.yaml"))
    kubectl("rollout", "status", "deploy/recommendation-service", "--timeout=120s")


def _deploy_11():
    sh(["git", "checkout", "scenario-11-pricing-n-plus-one-bug"])
    sh(["git", "checkout", "feat/per-sku-discount-pricing", "--",
        "environment/services/pricing-service"])
    sh(["bash", "scenarios/11-pricing-n-plus-one/deploy.sh"])


def _restore_11():
    kubectl("set", "image", "deploy/pricing-service",
            "pricing-service=byemini2/pricing-service:v2")
    kubectl("scale", "deploy/pricing-service", "--replicas=1")
    kubectl("rollout", "status", "deploy/pricing-service", "--timeout=120s")


SCENARIOS = [
    # billing-service is 2 hops downstream of api-gateway (api-gateway ->
    # checkout -> billing-service) — checkout itself is only 1 hop and was
    # deliberately not used.
    Scenario("09", "billing-service missing timeout", "chore/billing-http-client-pooling",
             _deploy_09, _restore_09, RUBRICS["09"], affected_service="API gateway"),
    # recommendation-service is 2 hops downstream of search-service
    # (search-service -> ranking-service -> recommendation-service) —
    # ranking-service/review-service are only 1 hop and were not used.
    Scenario("10", "recommendation-service OOM", "chore/service-resource-rightsizing",
             _deploy_10, _restore_10, RUBRICS["10"], affected_service="search service"),
    # pricing-service is 3 hops downstream of frontend (frontend ->
    # api-gateway -> cart-service -> pricing-service) — cart-service/
    # checkout/catalog-service are only 1 hop and were not used.
    Scenario("11", "pricing-service N+1", "feat/per-sku-discount-pricing",
             _deploy_11, _restore_11, RUBRICS["11"], affected_service="frontend"),
]


# ── GitHub helpers (branch fork + PR lookup) ─────────────────────────────
def gh_headers() -> dict:
    return {"Authorization": f"Bearer {GITHUB_TOKEN}", "Accept": "application/vnd.github+json"}


def check_github_push_access() -> None:
    r = httpx.get(f"https://api.github.com/repos/{GITHUB_OWNER_REPO}", headers=gh_headers())
    r.raise_for_status()
    if not r.json().get("permissions", {}).get("push"):
        raise RuntimeError(
            f"GITHUB_TOKEN cannot push to {GITHUB_OWNER_REPO} — "
            "agents would fail to open PRs. Fix the token's scope before running."
        )


def branch_sha(branch: str) -> str:
    r = httpx.get(f"https://api.github.com/repos/{GITHUB_OWNER_REPO}/git/ref/heads/{branch}",
                  headers=gh_headers())
    r.raise_for_status()
    return r.json()["object"]["sha"]


def create_branch(name: str, sha: str) -> None:
    r = httpx.post(f"https://api.github.com/repos/{GITHUB_OWNER_REPO}/git/refs",
                    headers=gh_headers(), json={"ref": f"refs/heads/{name}", "sha": sha})
    if r.status_code == 422 and "already exists" in r.text:
        return
    r.raise_for_status()


def create_pr(branch: str, title: str, body: str, base: str = "main") -> dict:
    # The agent's in-sandbox git credential is scoped to push only (see
    # _BASE_SYSTEM_PROMPT's comment on this) — it cannot authenticate a PR-create
    # call itself, confirmed by a scenario-10 dry run where both agents hit
    # `401: Bad credentials` from `gh pr create` and burned most of their
    # runtime on it before giving up. So the script opens the PR itself, with
    # the real token from .env, once it's confirmed the agent actually
    # pushed something (see run_agent_session).
    r = httpx.post(f"https://api.github.com/repos/{GITHUB_OWNER_REPO}/pulls",
                    headers=gh_headers(),
                    json={"title": title, "body": body, "head": branch, "base": base})
    r.raise_for_status()
    pr = r.json()
    return {"url": pr["html_url"], "number": pr["number"], "state": pr["state"], "title": pr["title"]}


def find_pr_for_branch(branch: str) -> dict | None:
    owner = GITHUB_OWNER_REPO.split("/")[0]
    r = httpx.get(f"https://api.github.com/repos/{GITHUB_OWNER_REPO}/pulls",
                  headers=gh_headers(), params={"head": f"{owner}:{branch}", "state": "all"})
    r.raise_for_status()
    items = r.json()
    if not items:
        return None
    pr = items[0]
    return {"url": pr["html_url"], "number": pr["number"], "state": pr["state"], "title": pr["title"]}


def fetch_branch_diff(base_sha: str, branch: str, max_chars: int = 200_000) -> str:
    # Deliberately NOT the PR's own diff (head vs `main`) — the neutral
    # branches these eval branches fork from have *already* diverged from
    # `main` by design (that's the whole regression), so a head-vs-main diff
    # drags in every one of those pre-existing differences (30+ unrelated
    # files, in a scenario-10 dry run) on top of whatever the agent actually
    # changed. Confirmed by hand that this buried the real fix past a 60K-char
    # truncation and produced a false FAIL from the grader. Comparing against
    # base_sha — the exact commit this branch was forked from — isolates
    # just the agent's own commits, cleanly: a scenario-10 dry run's real fix
    # was a 518-character diff once measured this way, not 95,803.
    r = httpx.get(f"https://api.github.com/repos/{GITHUB_OWNER_REPO}/compare/{base_sha}...{branch}",
                  headers={**gh_headers(), "Accept": "application/vnd.github.v3.diff"})
    r.raise_for_status()
    diff = r.text
    if len(diff) > max_chars:
        diff = diff[:max_chars] + f"\n\n... [diff truncated at {max_chars} chars]"
    return diff


# ── Independent PR grading ───────────────────────────────────────────────
# Deliberately NOT the same thing as the platform's own user.define_outcome
# grader (span.outcome_evaluation_end) captured in run_agent_session below.
# That grader's exact input isn't documented anywhere we could confirm while
# building this script — it might be evaluating sandbox file state, the
# conversation transcript, or the PR itself, and we didn't want to just
# assume the last one. This is a second, independent verdict that reads the
# ACTUAL PR diff off GitHub and grades only that, so "correct" in the final
# report always has at least one plain, auditable path: this prompt, this
# diff, this rubric, this response — nothing else in scope.
_GRADER_SYSTEM = (
    "You are grading a pull request's diff against a rubric describing what "
    "a correct fix looks like. Base your verdict ONLY on the diff shown — "
    "you have no other context about the incident. Respond with a single "
    "line 'VERDICT: PASS' or 'VERDICT: FAIL', then a short paragraph "
    "explaining why, citing specific lines/files from the diff."
)


def grade_pr_diff(diff: str, rubric: str) -> dict:
    resp = client.messages.create(
        model=MODEL,
        max_tokens=1024,
        system=_GRADER_SYSTEM,
        messages=[{"role": "user", "content": f"RUBRIC:\n{rubric}\n\nDIFF:\n{diff}"}],
    )
    text = "".join(b.text for b in resp.content if getattr(b, "type", None) == "text")
    first_line = text.strip().splitlines()[0] if text.strip() else ""
    verdict = "PASS" if "PASS" in first_line.upper() else "FAIL"
    return {"verdict": verdict, "explanation": text}


# ── Agent setup (mirrors agent.py, but parameterized per config instead of
# reading process-wide env vars, since this script runs BOTH configs in one
# process — and plain create-or-find-by-name instead of @st.cache_resource,
# which needs a Streamlit runtime this script doesn't have) ─────────────
@dataclass
class AgentConfig:
    key: str
    label: str
    enable_causely: bool
    agent_id: str | None = field(default=None)
    env_id: str | None = field(default=None)


def _find_by_name(items, name: str):
    for item in items:
        if item.name == name:
            return item
    return None


def system_prompt(enable_causely: bool) -> str:
    return _BASE_SYSTEM_PROMPT + (_CAUSELY_FIRST if enable_causely else "")


def tools_for(enable_causely: bool) -> list[dict]:
    tools = [
        {"type": "agent_toolset_20260401", "default_config": {"enabled": True}},
        {"type": "mcp_toolset", "mcp_server_name": "k8s",
         "default_config": {"enabled": True, "permission_policy": {"type": "always_allow"}}},
    ]
    if GRAFANA_MCP_URL:
        tools.append({"type": "mcp_toolset", "mcp_server_name": "grafana",
                       "default_config": {"enabled": True, "permission_policy": {"type": "always_allow"}}})
    if enable_causely:
        tools.append({"type": "mcp_toolset", "mcp_server_name": "causely",
                       "default_config": {"enabled": True, "permission_policy": {"type": "always_allow"}}})
    return tools


def mcp_servers_for(enable_causely: bool) -> list[dict]:
    servers = [{"type": "url", "name": "k8s", "url": K8S_MCP_URL}]
    if GRAFANA_MCP_URL:
        servers.append({"type": "url", "name": "grafana", "url": GRAFANA_MCP_URL})
    if enable_causely:
        servers.append({"type": "url", "name": "causely", "url": CAUSELY_MCP_URL})
    return servers


def setup_agent(cfg: AgentConfig) -> str:
    existing = _find_by_name(client.beta.agents.list(limit=100).data, cfg.label)
    if existing is not None:
        updated = client.beta.agents.update(
            existing.id, version=existing.version,
            system=system_prompt(cfg.enable_causely), tools=tools_for(cfg.enable_causely),
            mcp_servers=mcp_servers_for(cfg.enable_causely),
        )
        return updated.id
    created = client.beta.agents.create(
        name=cfg.label, model=MODEL, system=system_prompt(cfg.enable_causely),
        tools=tools_for(cfg.enable_causely), mcp_servers=mcp_servers_for(cfg.enable_causely),
    )
    return created.id


def setup_environment(cfg: AgentConfig) -> str:
    env_name = f"eval-{cfg.key}"
    existing = _find_by_name(client.beta.environments.list(limit=100).data, env_name)
    if existing is not None:
        return existing.id
    created = client.beta.environments.create(
        name=env_name, config={"type": "cloud", "networking": {"type": "unrestricted"}},
    )
    return created.id


def _fetch_causely_access_token() -> str:
    resp = httpx.post(
        os.environ["CAUSELY_TOKEN_URL"],
        json={"clientId": os.environ["CAUSELY_CLIENT_ID"], "secret": os.environ["CAUSELY_CLIENT_SECRET"]},
        timeout=10,
    )
    resp.raise_for_status()
    body = resp.json()
    for key in ("access_token", "accessToken", "token"):
        if key in body:
            return body[key]
    raise KeyError(f"no access token in Causely token response: {body!r}")


def setup_vault(enable_causely: bool) -> str:
    vault = client.beta.vaults.create(display_name="mcp-creds")
    if GRAFANA_MCP_URL and GRAFANA_TOKEN:
        client.beta.vaults.credentials.create(
            vault_id=vault.id,
            auth={"type": "static_bearer", "mcp_server_url": GRAFANA_MCP_URL, "token": GRAFANA_TOKEN},
        )
    if enable_causely:
        client.beta.vaults.credentials.create(
            vault_id=vault.id,
            auth={"type": "static_bearer", "mcp_server_url": CAUSELY_MCP_URL,
                  "token": _fetch_causely_access_token()},
        )
    return vault.id


# ── One agent's run on one scenario ──────────────────────────────────────
# Empirically (a scenario-10 dry run, see the 2026-09-15 session transcripts):
# a real investigate-diagnose-fix-PR cycle took ~18 minutes for the baseline
# agent and ~23 minutes for the Causely agent — both of which BLEW PAST a
# 10-minute client-side timeout despite the session itself going on to reach
# "satisfied" a few minutes later. The remote session doesn't stop just
# because we stop watching it, so a too-short timeout doesn't just mean "we
# report a false failure" — it means we walk away from a still-running,
# still-billing session without ever seeing its real result. Two fixes:
# generous default timeout (60 min - still not a guarantee, raise it further
# for slower scenarios), and on timeout we explicitly send `user.interrupt`
# so the remote session actually stops rather than being abandoned.
def run_agent_session(cfg: AgentConfig, branch: str, base_sha: str, rubric: str, prompt: str, timeout_s: int) -> dict:
    vault_id = setup_vault(cfg.enable_causely)
    resource = {
        "type": "github_repository",
        "url": GITHUB_REPO_URL,
        "mount_path": "/workspace/repo",
        "authorization_token": GITHUB_TOKEN,
        "checkout": {"type": "branch", "name": branch},
    }
    session = client.beta.sessions.create(
        agent=cfg.agent_id, environment_id=cfg.env_id, vault_ids=[vault_id], resources=[resource],
    )

    start = time.monotonic()
    outcome_result = None
    outcome_explanation = None
    error = None
    tool_calls = 0
    # `session.usage` events aren't in this SDK version's typed event union
    # (checked: not one of the ~35 types in beta_managed_agents_stream_session
    # _events.py) but the API emits them anyway and the Anthropic BaseModel's
    # extra="allow" config keeps the raw fields readable via getattr — so
    # this is a live, in-stream fallback for usage/cost/active-time, since
    # sessions.retrieve() below has been observed to return zeroed usage for
    # a session we stopped watching before it went idle server-side.
    last_usage: dict | None = None
    timed_out = False
    # `events.stream()` has no cursor/offset param (checked the SDK — plain
    # GET .../events/stream, nothing to resume from), so it replays a
    # session's full event history from the start every time it's opened.
    # That matters below: a reconnect re-tallies from zero rather than
    # double-counting whatever the first attempt already saw.
    reached_terminal = False
    reconnects = 0
    max_reconnects = 5
    while True:
        outcome_result = None
        outcome_explanation = None
        tool_calls = 0
        last_usage = None
        reached_terminal = False
        try:
            with client.beta.sessions.events.stream(session.id) as stream:
                if reconnects == 0:
                    client.beta.sessions.events.send(session.id, events=[{
                        "type": "user.define_outcome",
                        "description": prompt,
                        "rubric": {"type": "text", "content": rubric},
                        "max_iterations": 5,
                    }])
                for ev in stream:
                    if time.monotonic() - start > timeout_s:
                        timed_out = True
                        error = "timeout"
                        reached_terminal = True
                        break
                    t = ev.type
                    if t in ("agent.tool_use", "agent.mcp_tool_use", "agent.custom_tool_use"):
                        tool_calls += 1
                    elif t == "session.usage":
                        usage = getattr(ev, "usage", None)
                        if usage is not None:
                            last_usage = usage if isinstance(usage, dict) else usage.model_dump()
                    elif t == "span.outcome_evaluation_end":
                        outcome_result = ev.result
                        outcome_explanation = ev.explanation
                    elif t == "session.status_idle":
                        if outcome_result is not None and outcome_result != "needs_revision":
                            reached_terminal = True
                            break
                        if outcome_result is None:
                            # idle with no outcome verdict at all is unexpected —
                            # bail rather than hang forever
                            reached_terminal = True
                            break
                    elif t == "session.status_terminated":
                        error = error or "session_terminated_before_verdict"
                        reached_terminal = True
                        break
                    elif t == "session.error":
                        detail = getattr(getattr(ev, "error", None), "message", None) or str(ev)
                        error = f"session_error: {detail}"
                        reached_terminal = True
                        break
                if timed_out:
                    # Stop the remote session rather than leaving it running
                    # unattended (and unaccounted for) after we walk away.
                    try:
                        client.beta.sessions.events.send(
                            session.id, events=[{"type": "user.interrupt"}])
                    except Exception:  # noqa: BLE001 — best-effort, don't mask the timeout
                        pass
        except Exception as e:  # noqa: BLE001 — report, don't crash the whole run
            error = f"exception: {e}"
            reached_terminal = True

        if reached_terminal or timed_out:
            break

        # The stream ended (StopIteration) without ever hitting one of the
        # branches above — not idle, not terminated, not errored, not our
        # own timeout. That's not a completion signal, it's the connection
        # dropping: seen live on a scenario-10 run where the stream went
        # silent ~110s in while the session kept working server-side for
        # another ~11 minutes and later reached a genuine "satisfied"
        # verdict — previously misreported as NO_PR/no-fix because this code
        # trusted silent stream closure as "the agent is done". Confirm
        # against the session's own authoritative status before giving up.
        reconnects += 1
        if reconnects > max_reconnects:
            error = "stream_disconnected_repeatedly"
            break
        probe = client.beta.sessions.retrieve(session.id)
        if probe.status in ("idle", "terminated"):
            break  # genuinely done; the disconnect just raced the last events
        time.sleep(min(5 * reconnects, 30))

    full = client.beta.sessions.retrieve(session.id)

    # sessions.retrieve() also carries the authoritative, complete
    # outcome-evaluation state (one entry per define_outcome event sent).
    # Prefer its terminal verdict over whatever the stream captured, in case
    # every attempt above missed the live span.outcome_evaluation_end event
    # (e.g. it fired in the gap between a disconnect and the reconnect probe).
    if full.outcome_evaluations:
        latest = full.outcome_evaluations[-1]
        if latest.result in ("satisfied", "max_iterations_reached", "failed", "interrupted"):
            outcome_result = latest.result
            outcome_explanation = latest.explanation

    # The agent cannot open its own PR (see _BASE_SYSTEM_PROMPT's comment on the
    # sandbox git credential being push-only — confirmed by a dry run where
    # both agents hit `401: Bad credentials` from `gh pr create` and burned
    # most of their runtime on it). So: check whether the branch actually
    # moved past the commit we forked it from — that's the one fact that
    # tells us the agent really pushed something, as opposed to just talking
    # about a fix — and if so, open the PR ourselves with the real token.
    pr = find_pr_for_branch(branch)  # in case one already exists (idempotent reruns)
    if pr is None:
        try:
            new_sha = branch_sha(branch)
        except Exception:  # noqa: BLE001 — branch lookup failing isn't fatal, just means no PR
            new_sha = base_sha
        if new_sha != base_sha:
            try:
                pr = create_pr(
                    branch,
                    title=f"[eval:{cfg.key}] fix for scenario under investigation",
                    body=(f"Opened by eval_scenarios.py on behalf of {cfg.label} "
                          f"(session {session.id}), which pushed to `{branch}` but "
                          "cannot authenticate a PR-create call from inside its sandbox.\n\n"
                          f"Agent's own summary of its fix (if it reported one) is in the "
                          "session transcript."),
                )
            except Exception as e:  # noqa: BLE001 — report, don't crash the whole run
                error = error or f"pr_creation_failed: {e}"

    # Independent verdict: actually read the PR's diff and grade it against
    # the same rubric, separately from whatever the platform's own
    # define_outcome grader looked at (see grade_pr_diff's docstring-comment
    # above for why this exists at all). No PR found -> nothing to grade;
    # that's itself a FAIL-worthy fact, surfaced via diff_verdict staying None
    # rather than us inventing a verdict for a PR that doesn't exist.
    diff_grade = None
    if pr and pr["state"] != "closed":
        try:
            diff = fetch_branch_diff(base_sha, branch)
            diff_grade = grade_pr_diff(diff, rubric)
        except Exception as e:  # noqa: BLE001 — grading failure shouldn't sink the run
            diff_grade = {"verdict": "ERROR", "explanation": f"grading failed: {e}"}

    # Prefer the retrieved session's own numbers (populated once it's
    # properly idle/terminated); fall back to the last in-stream usage event
    # for a session we had to interrupt, since retrieve() returned zeros for
    # exactly that case during the scenario-10 dry run.
    input_tokens = full.usage.input_tokens or (last_usage or {}).get("input_tokens")
    output_tokens = full.usage.output_tokens or (last_usage or {}).get("output_tokens")
    active_seconds = full.stats.active_seconds or (last_usage or {}).get("active_seconds")
    cost = (last_usage or {}).get("list_cost")

    return {
        "session_id": session.id,
        "branch": branch,
        "outcome_result": outcome_result,
        "outcome_explanation": outcome_explanation,
        "error": error,
        "tool_calls": tool_calls,
        "duration_seconds": full.stats.duration_seconds,
        "active_seconds": active_seconds,
        "input_tokens": input_tokens,
        "output_tokens": output_tokens,
        "cost": cost,
        "pr": pr,
        "diff_verdict": diff_grade["verdict"] if diff_grade else "NO_PR",
        "diff_explanation": diff_grade["explanation"] if diff_grade else None,
    }


# ── Orchestration ────────────────────────────────────────────────────────
def run_scenario(scenario: Scenario, agents: list[AgentConfig], timeout_s: int) -> dict:
    print(f"\n=== [{scenario.key}] deploying: {scenario.name} ===")
    scenario.deploy()
    print(f"    settling {scenario.settle_sec}s for the symptom to appear...")
    time.sleep(scenario.settle_sec)

    branches = {}
    base_sha = branch_sha(scenario.neutral_branch)
    for cfg in agents:
        b = f"eval/scn{scenario.key}-{cfg.key}-{RUN_ID}"
        create_branch(b, base_sha)
        branches[cfg.key] = b
        print(f"    forked {b} from {scenario.neutral_branch}@{base_sha[:7]}")

    prompt = generic_prompt(scenario.affected_service)
    print(f"    launching {len(agents)} agent(s) concurrently...")
    results: dict[str, dict] = {}
    with ThreadPoolExecutor(max_workers=len(agents)) as ex:
        futs = {
            ex.submit(run_agent_session, cfg, branches[cfg.key], base_sha, scenario.rubric, prompt, timeout_s): cfg
            for cfg in agents
        }
        for fut in as_completed(futs):
            cfg = futs[fut]
            try:
                results[cfg.key] = fut.result()
            except Exception as e:  # noqa: BLE001
                results[cfg.key] = {"error": f"exception: {e}"}
            print(f"    [{scenario.key}] {cfg.label}: "
                  f"platform_verdict={results[cfg.key].get('outcome_result')} "
                  f"diff_verdict={results[cfg.key].get('diff_verdict')} "
                  f"pr={(results[cfg.key].get('pr') or {}).get('url')}")

    print(f"=== [{scenario.key}] restoring ===")
    scenario.restore()
    return results


def print_summary(report: dict, agents: list[AgentConfig]) -> None:
    print("\n" + "=" * 78)
    print("SUMMARY")
    print("=" * 78)
    for cfg in agents:
        print(f"\n{cfg.label}")
        print("-" * len(cfg.label))
        total_active = 0.0
        total_in = 0
        total_out = 0
        correct = 0
        total = 0
        for scenario in SCENARIOS:
            r = report["scenarios"].get(scenario.key, {}).get(cfg.key, {})
            if not r:
                continue
            total += 1
            # diff_verdict is the primary signal — it's the one that actually
            # read the PR's diff. platform_verdict (the built-in
            # define_outcome grader) is shown alongside for comparison, since
            # we don't have confirmation the two are grading the same thing.
            diff_verdict = r.get("diff_verdict") or "unknown"
            platform_verdict = r.get("outcome_result") or r.get("error") or "unknown"
            ok = diff_verdict == "PASS"
            correct += int(ok)
            active = r.get("active_seconds") or 0
            in_tok = r.get("input_tokens") or 0
            out_tok = r.get("output_tokens") or 0
            total_active += active
            total_in += in_tok
            total_out += out_tok
            cost = r.get("cost") or {}
            cost_str = f"{cost['amount']} {cost.get('currency', '')}".strip() if cost else "n/a"
            pr_url = (r.get("pr") or {}).get("url", "(no PR found)")
            print(f"  scenario {scenario.key} ({scenario.name})")
            print(f"    correct fix : {'YES' if ok else 'NO'}  "
                  f"(diff grade: {diff_verdict}; platform grade: {platform_verdict})")
            print(f"    PR          : {pr_url}")
            print(f"    active time : {active:.0f}s   tokens: {in_tok + out_tok} "
                  f"(in {in_tok} / out {out_tok})   cost: {cost_str}")
        print(f"  TOTAL: {correct}/{total} correct, "
              f"{total_active:.0f}s active time, {total_in + total_out} tokens "
              f"(in {total_in} / out {total_out})")
    print("\n" + "=" * 78)
    print(f"full report written to {REPORT_PATH}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenarios", default="09,10,11",
                         help="comma-separated scenario keys to run, e.g. '10' for a dry run")
    parser.add_argument("--timeout-min", type=int, default=60,
                         help="max wall-clock minutes per agent per scenario "
                              "(a scenario-10 dry run took ~18-23 min end to end; "
                              "don't set this below ~25 even for a quick test)")
    parser.add_argument("--settle-sec", type=int, default=None,
                         help="override every scenario's default settle time")
    parser.add_argument("--cooldown-sec", type=int, default=600,
                         help="wait between scenarios, after one is restored and before "
                              "the next deploys, so the just-resolved symptom/issue has "
                              "time to deactivate in Causely (~5 min observed) instead of "
                              "still looking 'recent' when the next scenario's agents call "
                              "get_issues. 600s (2x the observed ~5min) rather than cutting "
                              "it close — a scenario-11 dry run caught a still-forming "
                              "recommendation-service CrashFailure from scenario 10 minutes "
                              "into scenario 11's session at the old 300s default, costing "
                              "the Causely agent a full extra investigate-fix-revert cycle")
    args = parser.parse_args()

    keys = [k.strip() for k in args.scenarios.split(",") if k.strip()]
    scenarios = [s for s in SCENARIOS if s.key in keys]
    if not scenarios:
        sys.exit(f"no matching scenarios for --scenarios={args.scenarios!r}")
    if args.settle_sec is not None:
        for s in scenarios:
            s.settle_sec = args.settle_sec

    print("Checking GitHub token has push access...")
    check_github_push_access()

    print("Setting up agents...")
    causely_cfg = AgentConfig("causely", "Eval Agent (Causely)", True)
    baseline_cfg = AgentConfig("baseline", "Eval Agent (Baseline)", False)
    agents = [causely_cfg, baseline_cfg]
    for cfg in agents:
        cfg.agent_id = setup_agent(cfg)
        cfg.env_id = setup_environment(cfg)
        print(f"  {cfg.label}: agent={cfg.agent_id} env={cfg.env_id}")

    report = {
        "run_id": RUN_ID,
        "started_at": datetime.now(timezone.utc).isoformat(),
        "agents": {cfg.key: {"label": cfg.label, "agent_id": cfg.agent_id} for cfg in agents},
        "scenarios": {},
    }
    timeout_s = args.timeout_min * 60

    try:
        for i, scenario in enumerate(scenarios):
            report["scenarios"][scenario.key] = run_scenario(scenario, agents, timeout_s)
            REPORT_PATH.write_text(json.dumps(report, indent=2))  # save progress after each scenario
            if i < len(scenarios) - 1:
                print(f"    cooling down {args.cooldown_sec}s so the just-restored "
                      f"symptom deactivates in Causely before the next scenario deploys...")
                time.sleep(args.cooldown_sec)
    finally:
        report["finished_at"] = datetime.now(timezone.utc).isoformat()
        REPORT_PATH.write_text(json.dumps(report, indent=2))

    print_summary(report, agents)


if __name__ == "__main__":
    main()
