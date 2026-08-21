#!/usr/bin/env bash
# Show which tools an investigation actually used — the proof that Causely MCP is
# being consulted rather than the agent working from AWS telemetry alone.
#
#   scripts/show-tool-calls.sh              newest investigation
#   scripts/show-tool-calls.sh <task-id>    a specific one
#   scripts/show-tool-calls.sh --wait       poll until the newest one completes
#
# Reads the execution journal's "utilization" records, which carry per-tool call
# counts. Causely tools appear by name (get_diagnosis_details, get_topology, ...);
# the agent's built-ins appear as use_aws / use_kubectl.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

confirm_account
agent_space_id="$(require_state agent-space-id)"

wait_for_completion=0
task_id=""
for arg in "$@"; do
  case "$arg" in
    --wait) wait_for_completion=1 ;;
    *) task_id="$arg" ;;
  esac
done

# --- locate the investigation ----------------------------------------------

newest_task() {
  agent list-backlog-tasks --agent-space-id "$agent_space_id" 2>/dev/null | python3 -c '
import json, sys
data = json.loads(sys.stdin.read() or "{}")
items = next((v for v in data.values() if isinstance(v, list)), [])
items = [i for i in items if isinstance(i, dict) and i.get("executionId")]
if not items:
    raise SystemExit(0)
items.sort(key=lambda i: str(i.get("createdAt")), reverse=True)
top = items[0]
print("\t".join([
    top.get("taskId") or "",
    top.get("executionId") or "",
    top.get("status") or "",
    top.get("title") or "",
]))
'
}

find_task() {
  local wanted="$1"
  agent list-backlog-tasks --agent-space-id "$agent_space_id" 2>/dev/null | python3 -c '
import json, sys
data = json.loads(sys.stdin.read() or "{}")
wanted = sys.argv[1]
items = next((v for v in data.values() if isinstance(v, list)), [])
for item in items:
    if isinstance(item, dict) and item.get("taskId") == wanted:
        print("\t".join([
            item.get("taskId") or "",
            item.get("executionId") or "",
            item.get("status") or "",
            item.get("title") or "",
        ]))
        break
' "$wanted"
}

lookup() { [[ -n "$task_id" ]] && find_task "$task_id" || newest_task; }

row="$(lookup)"
[[ -n "$row" ]] || die "no investigations found for this agent space"
IFS=$'\t' read -r task_id execution_id status title <<<"$row"

info "$title"
ok "task $task_id"
ok "status $status"

if (( wait_for_completion )); then
  # Investigations typically take a few minutes.
  for _ in $(seq 1 40); do
    [[ "$status" == "COMPLETED" || "$status" == "FAILED" ]] && break
    sleep 15
    row="$(find_task "$task_id")"
    IFS=$'\t' read -r task_id execution_id status title <<<"$row"
    skip "still $status"
  done
  ok "final status $status"
fi

# --- read tool usage out of the journal ------------------------------------

echo
info "Tools used"

# Written to a file rather than piped: `python3 - <<'PY'` takes the heredoc as
# stdin, so a piped payload would never be readable.
journal_file="$(mktemp)"
trap 'rm -f "$journal_file"' EXIT
agent list-journal-records \
  --agent-space-id "$agent_space_id" \
  --execution-id "$execution_id" >"$journal_file" 2>/dev/null || echo '{}' >"$journal_file"

python3 - "$journal_file" <<'PY'
import json, sys

data = json.load(open(sys.argv[1]))
records = data.get("records") or []

# The most recent utilization record holds cumulative per-tool call counts.
tools, subagents = {}, {}
for record in records:
    if record.get("recordType") != "utilization":
        continue
    try:
        content = json.loads(record.get("content") or "{}")
    except json.JSONDecodeError:
        continue
    body = content.get("data") or {}
    for tool in body.get("tools") or []:
        name = tool.get("name")
        if name:
            tools[name] = max(tools.get(name, 0), tool.get("tool_use_count") or 0)
    for sub in body.get("subagents") or []:
        if sub.get("id"):
            subagents[sub["id"]] = sub.get("utilization") or 0

if not tools:
    print("  (no tool-usage records yet — the investigation may still be starting)")
    raise SystemExit(0)

builtin = {"use_aws", "use_kubectl", "fs_read", "fs_write", "report"}
causely = {name: count for name, count in tools.items() if name not in builtin}

for name, count in sorted(tools.items(), key=lambda kv: -kv[1]):
    tag = "" if name in builtin else "   <- Causely MCP"
    print(f"  {count:>4}x {name}{tag}")

print()
if causely:
    total = sum(causely.values())
    print(f"  Causely MCP was consulted: {total} call(s) across {len(causely)} tool(s)")
else:
    print("  Causely MCP was NOT consulted — the agent used only its built-in tools.")
    print("  Check the MCP association and consider adding a custom skill that tells")
    print("  the agent to consult Causely first for Kubernetes causality.")

if subagents:
    print()
    print("  subagents: " + ", ".join(sorted(subagents)))

# The conclusion itself, so the whole check is one command.
summaries = [r for r in records if r.get("recordType") == "investigation_summary_md"]
findings = [r for r in records if r.get("recordType") == "finding"]
if summaries or findings:
    print()
    print("--- conclusion ---")
for record in (summaries or findings)[:1]:
    content = record.get("content") or ""
    try:
        parsed = json.loads(content)
        content = parsed if isinstance(parsed, str) else json.dumps(parsed, indent=2)
    except json.JSONDecodeError:
        pass
    text = str(content).strip()
    print(text[:2500] + ("\n  ... (truncated)" if len(text) > 2500 else ""))
PY
