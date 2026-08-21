# What the two flows look like

Setup lives in [`DEPLOYMENT.md`](DEPLOYMENT.md). This page is what you actually see once it runs,
and what to check when a run looks right but isn't.

The single idea worth watching for: the agent is not guessing. Causely has already computed the
causal chain, and the agent's job is to act on it rather than re-derive it from raw telemetry.

## What a real run produced

A notification built from a live Causely diagnosis — a Critical payment-gateway failure — produced
an investigation that made **15 Causely MCP calls across 10 tools**, traced the cascade
`payment-gw → checkout-api → storefront-bff → web-client`, and identified the root cause as a Helm
deploy that switched a dependency into a rejection mode.

After granting read-only kubectl access ([`DEPLOYMENT.md` step 11](DEPLOYMENT.md#11-optional-read-only-cluster-access))
the same scenario got materially sharper. The agent made 11 `use_kubectl` calls alongside 17 Causely
calls and pinned the root cause to a specific Helm revision promoting a new image tag, noting that
the two backpressure environment variables were **unchanged** between revisions — so the regression
was code behaviour, not configuration. It could not have shown that without cluster access, and it
is the clearest argument for granting it.

## Before you run either flow

```bash
scripts/06-verify.sh
```

Expect: Agent Space active, an `aws` monitor association, the Causely MCP server listed under
registered services, the relay deployed with `DRY_RUN: false`, and an aggregator index present.

Then two checks that catch the failure modes which look like success:

- [ ] **Causely has a live diagnosis.** Run `scripts/make-fixture.sh` — it prints the one it chose.
      If it reports none active, the proactive flow has nothing real to fire on.

      **Do not skip this.** A stale or synthetic diagnosis id is the one failure that looks like a
      working system and isn't: the agent queries Causely, finds no such problem, and concludes —
      correctly, and at length — that the alert cannot be corroborated. Impressive reasoning,
      useless result.

- [ ] **MCP is genuinely wired.** In the operator web app chat, ask *"List the current Causely
      diagnoses."* and confirm the transcript shows a Causely MCP tool call. If this fails, both
      flows are hollow — fix it before going further.

      Chat cannot be driven from a shell: `SendMessage` is an event-stream operation and the AWS CLI
      does not expose event-stream operations. Use the web app here.

## Flow 1 — Proactive

Nobody is watching. Causely notices something and an agent picks up the work before an engineer is
involved.

**1. Start in Causely.** Look at the active diagnosis — the root cause, the affected entity, the
blast radius. This is the causal analysis the agent gets to start from rather than reproduce.

**2. Fire the notification.** Causely → Settings → Notifications → **Send Test Notification** is the
honest version, since nothing is staged. To send a specific diagnosis through the relay instead:

```bash
tests/send-test-notification.sh tests/fixtures/causely-live.json --fresh
```

`--fresh` matters on a second run. The agent deduplicates on `incidentId`, which the relay maps from
Causely's `objectId`, so re-sending the same fixture folds into the existing investigation instead of
opening a new one.

**3. Watch the investigation open.** In the web app a new investigation appears, its priority
reflecting Causely's severity. Open the timeline and watch the Causely MCP tool calls land — they
appear prefixed with the server name, e.g. `Causely_get_diagnosis_details`. That call is the whole
point: the agent is calling back into Causely with the id from the notification, picking up the
causal chain rather than re-deriving one from metrics.

From a terminal, or after the fact:

```bash
scripts/show-tool-calls.sh          # newest investigation
```

It prints per-tool call counts and the investigation's conclusion, and says plainly whether Causely
was consulted at all.

**4. Read the result.** The **Root Cause** tab, and a four-phase mitigation plan — Prepare,
Pre-Validate, Apply, Post-Validate. If Slack is wired, the summary lands there too.

Note what the agent did *not* do: it produced a plan, not a change. It has no write access anywhere
(see [What the agent is allowed to do](README.md#what-the-agent-is-allowed-to-do)), so a human
executes the mitigation. That is the guardrail, and it is why the proactive path needs no approval
gate.

## Flow 2 — Reactive

The ordinary case. Something feels wrong and an engineer just asks.

**1. Ask in plain language.** A fresh chat in the operator web app. Use a real symptom rather than a
service name — the point is that the engineer does not already know where to look:

> *"Checkout is slow and customers are complaining. What's actually going on, and what should I do
> about it?"*

**2. Watch the tool calls.** Typically `name_lookup`, `get_service_summary`, `get_symptoms`,
`get_diagnosis_details`, `get_topology` — resolve the service, check its health, pull the symptoms,
then ask Causely for the causal chain. What it is not doing is paging through dashboards hoping to
spot a correlation.

**3. Follow up.** Good ones, if the answer holds up:

- *"What else is affected?"* → blast radius
- *"Has this happened before?"* → history
- *"Write me a postmortem."* → the `postmortem` tool

Same engine as the proactive flow, different entry point. Proactive when Causely notices first,
reactive when a human does. Either way the agent reasons over Causely's causality instead of raw
telemetry.

## Making tool selection deterministic

The most common disappointment is an investigation that answers competently from CloudWatch alone
and never touches Causely. Nothing is broken — the agent simply had no reason to prefer one source
over another.

The fix is a **custom skill**: a Markdown runbook authored in the operator console, telling the agent
to consult Causely first for Kubernetes and application-layer causality. Worth adding before you
rely on either flow; tool selection left to chance is not a property you want in a demo or a page.

## If something looks wrong

| Symptom | Cause | Action |
|---|---|---|
| No investigation appears | Notification never arrived | `aws logs tail /aws/lambda/causely-devops-agent-relay --region us-east-1 --since 5m` |
| Relay returns 502 | Webhook url/secret wrong | `tests/send-direct-webhook.sh` isolates the agent side |
| Relay returns 401 | Bearer token mismatched | Compare with `.state/causely-notif-token`; check the `Bearer ` prefix |
| Relay returns 500, no Lambda logs | API Gateway cannot invoke the function | Re-run `scripts/05-deploy-relay.sh`; it re-adds the invoke permission |
| Investigation runs but never calls Causely | Tools not allowlisted, or MCP auth expired | Agent Space → Capabilities → MCP Servers |
| MCP calls fail mid-investigation | OAuth credentials rotated | No in-place update — deregister and re-register |
| Agent answers from CloudWatch only | No reason to prefer Causely | Add the custom skill, above |
| Agent says the alert cannot be corroborated | Synthetic or resolved diagnosis | `scripts/make-fixture.sh`, then re-send |
| Causely has no active problems | Quiet tenant | `tests/send-test-notification.sh` with a bundled fixture, accepting the caveat above |

## Afterwards

Nothing to tear down between runs; investigations accumulate harmlessly. Close the ones you created
so the next run starts clean, and remember each is billed by agent-second — don't leave the relay
taking real traffic once you are done. Full teardown is in
[`DEPLOYMENT.md`](DEPLOYMENT.md#tearing-down).
