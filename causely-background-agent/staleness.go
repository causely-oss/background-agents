package main

import (
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
)

// checkRootCauseStillActive asks the Causely MCP server whether rootCauseID is
// still an open issue before spending anything investigating it. A trigger can
// reach this agent well after the root cause was detected — a delayed Slack
// "Fix it" click, a manual/test POST /trigger, or simply an issue that resolved
// itself between detection and now. The poll trigger source already filters on
// only_active (see poll.go), but the webhook and Slack paths have no equivalent
// guard; this closes that gap once, uniformly, right before an investigation
// starts, regardless of trigger source.
//
// This fails open: get_issue_details's response schema isn't formally pinned
// down (same caveat as poll.go's polledIssue), so a tool-call error or an
// unparseable response lets the investigation proceed rather than being
// silently blocked by a check this agent can't fully rely on.
func checkRootCauseStillActive(client *mcpClient, rootCauseID string, log *zap.Logger) (skip bool, reason string) {
	raw, err := client.CallTool("get_issue_details", map[string]any{"issue_id": rootCauseID})
	if err != nil {
		log.Warn("staleness check: get_issue_details failed, proceeding anyway", zap.Error(err))
		return false, ""
	}

	var wrapped struct {
		Issues []struct {
			EndedAt string `json:"ended_at"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapped); err != nil || len(wrapped.Issues) == 0 {
		log.Warn("staleness check: could not parse get_issue_details response, proceeding anyway", zap.Error(err))
		return false, ""
	}

	if endedAt := wrapped.Issues[0].EndedAt; endedAt != "" {
		return true, fmt.Sprintf("root cause already resolved (ended_at=%s)", endedAt)
	}
	return false, ""
}
