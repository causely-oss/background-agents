<!-- Copyright 2026 Anthropic PBC -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Optional: triggering investigations from Causely

This is optional and separate from `ENABLE_CAUSELY` (which just gives the
chat agent Causely *tools* to call when you ask it something). This wires
the other direction: Causely pushes a notification, and that starts an
agent investigation with nobody watching — proactive instead of reactive.

Requires `ENABLE_CAUSELY=1` already working (see the main README) — the
agent needs the Causely MCP toolset to act on the id the notification
carries.

## Why a separate process from the Streamlit UI

Streamlit is request/response for a browser; it has no way to receive a
push from an external service. `webhook.py` is a small always-on FastAPI
app that does — a POST handler, nothing else. It imports `agent.py`
directly, so it starts sessions the exact same way the chat UI does.
`setup_agent()`/`setup_environment()` find-by-name before creating, so this
process and `app.py` converge on the **same** cloud agent and environment
even though they're separate processes — sessions started from a Causely
notification show up in the same session picker as everything else.

## The Managed Agents API has no native inbound-webhook trigger

Worth knowing before you go looking for one: the Managed Agents API has a
`deployments` resource with `schedule` (cron) and `manual` (fixed template,
run-on-demand) triggers, and a separate `client.beta.webhooks.unwrap()`
helper — but that's for *outbound* notifications Anthropic sends you about
session state, the reverse direction. There's no way to hand Causely a URL
and have Anthropic's platform receive it directly with per-call payload
content. `webhook.py` is the receiving side you have to host yourself,
same as any other inbound webhook.

## 1. Set a shared secret

```bash
CAUSELY_WEBHOOK_SECRET=<pick a long random string>
```

This is compared against whatever Causely sends back as the notification's
`Authorization` header — it's not a Causely-issued value, you choose it.

## 2. Run webhook.py + tunnel

```bash
./scripts/run-webhook.sh
```

Same pattern as `scripts/run-k8s-mcp.sh`, just in the opposite direction:
there the cloud agent needed to reach a local MCP server, here an external
service (Causely) needs to reach this local receiver. It prints a URL like:

```
Causely webhook URL: https://<tunnel-host>.trycloudflare.com/webhook/causely
```

That URL rotates every restart, same caveat as `K8S_MCP_URL` — see
[Durable webhook URL](#durable-webhook-url) below to stop that. For
anything beyond local testing, this still needs a real, stable deployment
instead of a laptop process behind a tunnel of either kind — a named
tunnel just gets you a fixed URL in the meantime.

## Durable webhook URL

The default quick tunnel above mints a random `*.trycloudflare.com`
hostname every time the script restarts. A cloudflared **named tunnel**
fixes that — same tool, but tied to a hostname you own instead of a random
one, so it survives restarts. One-time setup, against your own Cloudflare
account and a domain already managed there:

```bash
# 1. Authorize cloudflared against your Cloudflare account (opens a
#    browser). Writes ~/.cloudflared/cert.pem.
cloudflared tunnel login

# 2. Create the tunnel. Writes credentials to ~/.cloudflared/<tunnel-id>.json.
cloudflared tunnel create causely-webhook

# 3. Point a hostname in your Cloudflare-managed domain at it (a CNAME).
cloudflared tunnel route dns causely-webhook causely-webhook.yourdomain.com
```

Then run the script with that tunnel's name and hostname instead of the
default quick-tunnel mode:

```bash
WEBHOOK_TUNNEL_NAME=causely-webhook \
WEBHOOK_TUNNEL_HOSTNAME=causely-webhook.yourdomain.com \
./scripts/run-webhook.sh
```

It'll print `https://causely-webhook.yourdomain.com/webhook/causely` —
paste that into Causely **once**; it won't change on subsequent restarts,
so there's nothing to re-paste. Steps 1–3 only need to happen once ever
(the credentials in `~/.cloudflared/` persist); after that, just set the
two env vars whenever you run the script — an env file you `source`, or a
tiny wrapper script, works well if you don't want to retype them.

## 3. Point Causely at it

In Causely: **Settings → Notifications → Generic**, set the URL to the
`/webhook/causely` address above, and the auth token to
`CAUSELY_WEBHOOK_SECRET`.

**Send Issues, not defects.** An Issue groups the related diagnoses for an
affected entity into one incident with a designated primary diagnosis —
the right granularity for one investigation. A defect is one finding
beneath it. `webhook.py` handles either (`object_type` on the payload
selects `get_issue_details` vs `get_diagnosis_details`), but Issues are
what you want feeding an agent.

**Filter by severity.** Without a severity filter on the notification
config, a noisy afternoon of Low-severity issues means a lot of billed
investigations. Critical/High only is the sane default.

## What happens on a notification

1. Causely POSTs its notification payload verbatim (no templating on its
   side) to `/webhook/causely`.
2. `webhook.py` checks the bearer token, then translates the payload into
   the agent's first message — carrying the Causely `objectId` and
   `object_type` through as an explicit instruction to call
   `get_issue_details`/`get_diagnosis_details` with that id *first*, so the
   agent starts from Causely's already-computed causal chain instead of
   re-deriving one from raw k8s/Grafana state.
3. It starts a new session (`agent.start_session()`) and returns `200`
   immediately — the investigation itself runs in a background task, since
   nothing is waiting on the HTTP response.
4. Review the result the same way as any manual session: `streamlit run
   app.py`, session picker.

## Verify

```bash
curl -s http://localhost:8082/healthz
```

Then send a real Causely Issue payload (not a synthetic one — see the
`tests/fixtures/` note in `../aws-devops-agent/README.md` for why a made-up
`objectId` just gets you a correct "I called Causely and it found nothing"
result) with the configured token:

```bash
curl -X POST http://localhost:8082/webhook/causely \
  -H "Authorization: Bearer $CAUSELY_WEBHOOK_SECRET" \
  -H "Content-Type: application/json" \
  --data @payload.json
```

A `401` means the token doesn't match; check `webhook.py`'s log for which
session it started and watch it show up in the Streamlit session picker.
