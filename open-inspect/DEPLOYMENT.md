# Deploying Open-Inspect for automated remediation with Causely

This walks through standing up a **self-hosted Open-Inspect instance** from scratch and wiring it
to Causely, so that a real Kubernetes root cause automatically triggers an investigation grounded
in Causely's MCP evidence and — where warranted — opens a GitHub PR with the fix.

If you already have Open-Inspect running, skip to [`README.md`](README.md) instead — that's the
short version covering only the Causely-specific wiring.

## What you end up with

```
Causely root cause  →  webhook  →  Open-Inspect control plane  →  sandboxed agent session
                                                                        │
                                                                        ├─ Causely MCP tools
                                                                        │  (root cause, logs,
                                                                        │   topology, evidence)
                                                                        └─ GitHub PR with the fix
```

Open-Inspect ([`ColeMurray/background-agents`](https://github.com/ColeMurray/background-agents))
is self-hosted on Cloudflare Workers + Durable Objects + D1, with sandboxed agent execution on a
provider of your choice (Modal, Daytona, Vercel Sandbox, or OpenComputer — this guide uses Modal).
Its agent runtime, OpenCode, already supports remote MCP servers via config, so pointing it at
Causely's MCP server is a config change, not a fork or code contribution.

## Prerequisites

You'll need accounts / access for:

- **Cloudflare** — hosts the control plane and (optionally) the web UI
- **Modal** (or your chosen sandbox provider) — runs the ephemeral agent sessions
- **GitHub** — a GitHub App with write access to the repo(s) you want investigated
- **Anthropic** — an API key for the agent's model calls
- **Terraform** ≥ v1.14.0 — deploys everything above
- A **Causely** tenant with MCP access (see [`../../mcp/`](../../mcp/) for the MCP server itself)


Below is a Open-Inspect + Causely deployment linear checklist. Work top to bottom. Each step is self-contained — no jumping around.

## ☐ 1. Cloudflare account setup
 
- [ ] Sign up at dash.cloudflare.com (if you haven't already)
- [ ] Note your **Account ID** and **Workers subdomain**: Workers & Pages → Overview → Account Details panel
- [ ] Enable R2: Storage & databases → R2 → Enable R2 (requires adding billing info; first 10GB/month free)
- [ ] Create an R2 bucket for Terraform state, e.g. `open-inspect-terraform-state`
- [ ] Create **Token 1** (R2 token): on the R2 page → Manage API Tokens → Create → permission **Object Read & Write**. Save the Access Key ID + Secret Access Key (S3-compatible pair).
- [ ] Create **Token 2** (main token): profile icon → My Profile → API Tokens → Create Token → template **"Edit Cloudflare Workers"** → set Workers Scripts, Workers KV Storage, and Workers R2 Storage to **Edit** (already included by the template).
  - **The template does NOT include D1 or Queues — you must add these manually** or `terraform apply` will fail with 401 errors on `cloudflare_d1_database` and `cloudflare_queue` resources:
    - Click **+ Add more** → Account → **D1** → Edit
    - Click **+ Add more** → Account → **Workers Queues** (or whatever the dropdown labels it — search "queue") → Edit
  - Scroll down and complete the full save/create flow, then copy the token value.

Note, at real-world request volumes for a single team, expect this to stay within Cloudflare Workers'
free tier (100,000 requests/day) — the request counter includes the web UI's own dashboard
polling, which can look higher than actual traffic at a glance.


## ☐ 2. Modal account setup
 
- [ ] Sign up at modal.com (GitHub OAuth is fastest)
- [ ] Run:
```bash
  pip3 install modal
  modal setup
```
  This authorizes via browser and writes your workspace name + token pair to `~/.modal.toml`.
 

 ## ☐ 3. GitHub App
 
- [ ] Go to `github.com/settings/apps/new`
  - **Name**: anything unique
  - **Homepage URL**: anything valid
  - **Callback URL**: `https://example.com/callback` (placeholder — corrected in step 8 below)
  - **Uncheck "Active"** under the Webhook section
- [ ] Under **Repository permissions**: set Contents → Read & write, Pull requests → Read & write
- [ ] Under **Account permissions**: set **Email addresses** → Read-only. 
- [ ] Scroll down, click **Create GitHub App** (this saves everything above in one click)
- [ ] On the same settings page: **Client secrets** → Generate a new client secret → copy immediately (shown once)
- [ ] Same page: **Private keys** → Generate a private key → downloads a `.pem` file
- [ ] Left sidebar → **Install App** → select the target repo(s)
- [ ] After install, check the browser URL bar: `github.com/settings/installations/12345678` — that number is your **Installation ID**
- [ ] Convert the private key (Terraform needs PKCS#8, GitHub gives PKCS#1):
```bash
  openssl pkcs8 -topk8 -inform PEM -outform PEM -nocrypt -in <downloaded>.pem -out <converted>.pem
```
  Use `<converted>.pem` going forward, not the original download.
 
**You now have:** App ID, Client Secret, Installation ID, converted private key.


## ☐ 4. Anthropic API key
 
- [ ] Create an API key at console.anthropic.com


## ☐ 5. Terraform CLI
 
- [ ] Check `terraform/environments/production/versions.tf` in the Open-Inspect repo for the minimum version (≥ v1.14.0 as of writing)
- [ ] If your installed version is older, install from HashiCorp directly:
```bash
  curl -sSL -o terraform.zip https://releases.hashicorp.com/terraform/<version>/terraform_<version>_darwin_arm64.zip
  curl -sSL -o terraform_SHA256SUMS https://releases.hashicorp.com/terraform/<version>/terraform_<version>_SHA256SUMS
  grep darwin_arm64 terraform_SHA256SUMS   # compare against: shasum -a 256 terraform.zip
  unzip terraform.zip -d ~/bin && chmod +x ~/bin/terraform
```

## ☐ 6. Clone repo and fill in config
 
- [ ] Clone and prep config files:
```bash
  git clone https://github.com/ColeMurray/background-agents.git
  cd background-agents/terraform/environments/production
  cp terraform.tfvars.example terraform.tfvars
  cp backend.tfvars.example backend.tfvars
```
- [ ] Generate required secrets:
```bash
  openssl rand -base64 32   # → token_encryption_key, repo_secrets_encryption_key,
                            #   internal_callback_secret, nextauth_secret (run 4x, one per variable)
  openssl rand -hex 32      # → modal_api_secret, github_webhook_secret (run 2x)
```
- [ ] Open `terraform.tfvars` and `backend.tfvars` and fill in every value gathered so far: Cloudflare tokens/Account ID, GitHub App values, Anthropic key, Modal token, and the generated secrets above
- [ ] Set these decisions in `terraform.tfvars`:
  - `web_platform = "cloudflare"` (or `"vercel"`)
  - `sandbox_provider = "modal"` (or your provider)
  - a unique `deployment_name`
  - at least one allowlist entry: `allowed_users`, `allowed_email_domains`, `allowed_emails`, or `allowed_github_orgs`
  - `enable_durable_object_bindings = false`
  - `enable_service_bindings = false`
  (The two binding flags stay `false` for now — flipped on in step 8, phase 2.)


## ☐ 7. Build the code
 
From the repo root:
 
```bash
npm install -g wrangler   # plus Python 3.12+ and uv, if not already installed
npm install
npm run build -w @open-inspect/shared
cd packages/modal-infra && uv sync --frozen && cd -
npm run build -w @open-inspect/control-plane -w @open-inspect/slack-bot \
  -w @open-inspect/github-bot -w @open-inspect/linear-bot
```
 
(The web app itself builds automatically during `terraform apply` — no manual step for it.)


## ☐ 8. Deploy — Phase 1 (bindings off)
 
```bash
cd terraform/environments/production 
terraform init -backend-config=backend.tfvars
terraform plan -out=phase1.tfplan   # review before applying
terraform apply phase1.tfplan
```
 
- [ ] Check the health-check URLs printed in the plan output: control plane `/health`, Modal `/api_health`, and the web app root should all return 200
- [ ] Copy the real **web worker URL** from the output (format: `https://open-inspect-web-<deployment_name>.<workers-subdomain>.workers.dev`)
- [ ] Go back to the GitHub App settings page and replace the placeholder Callback URL with:
```
  https://open-inspect-web-<deployment_name>.<workers-subdomain>.workers.dev/api/auth/callback/github
```
  Save.


## ☐ 9. Deploy — Phase 2 (bindings on)
 
- [ ] In `terraform.tfvars`, flip both flags:
```
  enable_durable_object_bindings = true
  enable_service_bindings = true
```
- [ ] Apply again:
```bash
  terraform plan -out=phase2.tfplan
  terraform apply phase2.tfplan
```
- [ ] Re-check the same health checks
- [ ] Confirm `/sessions` now returns **401 unauthenticated** (proves auth is live)
> Note: `cloudflare_workers_cron_trigger` can't be destroyed via Terraform — expect a warning on every apply/destroy. Delete it manually via the Cloudflare dashboard if you ever tear this down.


## ☐ 10. Wire in Causely's MCP server
 
- [ ] Add `opencode.json` to the root of the repo you want investigated (OpenCode auto-loads project-root config):
```json
  {
    "$schema": "https://opencode.ai/config.json",
    "mcp": {
      "causely": {
        "type": "remote",
        "url": "https://api.causely.app/mcp",
        "enabled": true
      }
    }
  }
```
- [ ] If your Causely tenant needs machine credentials instead of browser OAuth, use `setup.sh` as a `.openinspect/setup.sh` hook instead of the static file above — it drops `opencode.json` into the sandbox at session start and, if `CAUSELY_MCP_CLIENT_BASIC` is set as a secret, injects it as an auth header. See [MCP Server Integration docs](https://docs.causely.ai/agent-integration/mcp-server/#authentication).


## ☐ 11. Create the automation
 
Open **`web_app_url` from step 8** (your Open-Inspect web UI, e.g. `https://open-inspect-web-<deployment_name>.<workers-subdomain>.workers.dev`) in a browser:
 
- [ ] Sign in via GitHub OAuth — you'll land on the repo/branch/model selector:
  ![Open-Inspect web UI landing page](screenshots/OpenInspect_webui.png)
- [ ] **Create Automation** → **Trigger Type: Inbound Webhook**:
  ![Blank Create Automation form, trigger-type picker](screenshots/CreateAutomation.png)
- [ ] **Repository Configuration** → bind to your target repo
- [ ] **Instructions** → use the template in `automation-instructions.md`
- [ ] Click **Create Automation** — the UI reveals the automation's **Webhook URL** and a per-automation **API Key** exactly once. Store both; you'll need the API key as a Bearer token on every trigger call.
  ![Post-creation reveal of the webhook URL and API key](screenshots/AutomationCreated.png)
Once saved, the automation's detail page shows its configuration, the full instructions text, and run history:
 
![Saved automation detail page — trigger type, model, instructions, run history](screenshots/ActualAutomationTrigger.png)


## ☐ 12. Connect Causely's alerts to the automation
 
- [ ] Point Causely's root-cause notifications at the automation's webhook:
```
  POST <control-plane-url>/webhooks/automation/<id>
  Authorization: Bearer <api-key>
```
  using the trigger payload shape and idempotency-key guidance in `automation-instructions.md`.
 
**Known limitation:** if you're using Causely's own mediator component to deliver alerts, its trigger-webhook configuration supports exactly one destination URL with no custom-header support — so it can't attach the automation's Bearer token directly. Until that's resolved, either add a small relay that injects the header, or trigger the webhook manually / via your own alerting glue for now. This doesn't affect anything else in this guide — the agent investigation and MCP evidence gathering work identically either way.
 
 
## ☐ 13. Verify with a real issue
 
- [ ] Don't fabricate a test payload if you can help it — reintroduce a real, known-fixable bug (e.g. a bad image tag in a manifest) and let Causely's analysis engine detect and fire the root cause naturally (this can take several minutes, not instant)
- [ ] Pull the real root cause's details via Causely's MCP tools (`get_issues` / `get_issue_details`) rather than inventing IDs or evidence, then trigger the automation with that real data
- [ ] Confirm in the Open-Inspect UI's session transcript that the agent calls `causely__get_issue_details` / `causely__get_logs` (or similar) **before** it touches source — that's the signal the MCP wiring is actually influencing the investigation, not just loaded and ignored
Right after triggering, the session transcript shows the raw webhook payload injected as untrusted context, and the sandbox spinning up:
 
![Session transcript: webhook JSON injected, sandbox spawning](screenshots/WebhookTriggered_SandboxSpawning.png)
 
You can independently cross-check that real infrastructure is doing the work — not just the UI claiming so — on your sandbox provider's own dashboard:
 
![Modal dashboard: sandbox creation in progress](screenshots/ModalSandboxSpawning.png)
![Modal dashboard: sandbox creation succeeded](screenshots/ModalSandboxSucceeded.png)
 
The key evidence is the agent actually calling Causely's MCP tools with real arguments before touching source — this is what distinguishes "grounded in real evidence" from "guessed from the manifest":
 
![Session transcript: real causely__get_root_cause_details / get_entity_health / get_logs calls](screenshots/CauselyMCP.png)
![Expanded tool-call detail: real root cause ID, causal chain, entity health](screenshots/mcpdetails.png)
 
And the result — a real PR, citing the real root-cause ID and the actual evidence that led to the diagnosis:
 
![Final session summary card: cost, files changed, PR link](screenshots/OpenPR.png)
![The resulting GitHub PR](screenshots/githubpr.png)
 
---
 
## Troubleshooting quick reference
 
| Symptom | Likely cause / fix |
|---|---|
| Session shows "Sandbox: failed" but sandbox provider dashboard shows success | Timing issue right after deploy or flag flip — retry once. Check `modal app logs <app-name>` (via `uv run modal app logs <app-name>` from `packages/modal-infra`) and `wrangler tail <control-plane-worker-name>` (needs `CLOUDFLARE_API_TOKEN` set) |
| Git clone inside sandbox fails with 401 | Per-session token can fail to propagate right after redeploy — retry with a fresh `idempotency_key` |
| OAuth callback fails after first deploy | Double-check the Callback URL matches the **`-web-`** worker's real URL, not the control-plane worker's URL |
 
---
 
## Tearing down
 
```bash
terraform destroy   # from terraform/environments/production/
```
 
This removes Workers, Durable Objects, D1, and any Cloudflare-managed R2 media bucket. It does **not** remove:
 
- [ ] The Terraform-state R2 bucket — delete manually if no longer needed
- [ ] Both Cloudflare API tokens — revoke manually
- [ ] Sandbox provider token — revoke if workspace won't be reused
- [ ] The GitHub App — uninstall (and optionally delete)
- [ ] The Anthropic API key — revoke if created solely for this deployment
- [ ] `terraform.tfvars` / `backend.tfvars` locally — delete if credentials inside should no longer exist anywhere
