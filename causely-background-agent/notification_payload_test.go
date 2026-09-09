package main

import (
	"encoding/json"
	"testing"
)

func TestNotificationPayload_ToTriggerPayload(t *testing.T) {
	raw := `{
		"labels": {"causely.ai/namespace": "chaosmania", "causely.ai/github-repo": "org/repo"},
		"entity": {"id": "e-1", "name": "buggy-app", "type": "Workload"},
		"name": "ImagePullErrors",
		"severity": "Critical",
		"objectId": "rc-1",
		"description": {
			"summary": "bad image tag",
			"remediationOptions": [{"title": "fix", "description": "correct the tag"}]
		}
	}`

	var n NotificationPayload
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := n.toTriggerPayload()

	if p.IssueID != "rc-1" || p.EntityID != "e-1" || p.EntityName != "buggy-app" ||
		p.RootCauseName != "ImagePullErrors" || p.Severity != "Critical" ||
		p.Description != "bad image tag" || p.Remediation != "correct the tag" ||
		p.EntityNamespace != "chaosmania" || p.GitHubRepoLabel != "org/repo" {
		t.Errorf("toTriggerPayload() = %+v", p)
	}
}

func TestNotificationPayload_CustomNamePreferredOverName(t *testing.T) {
	n := NotificationPayload{Name: "ImagePullErrors", CustomName: "Nginx image tag typo"}
	if got := n.toTriggerPayload().RootCauseName; got != "Nginx image tag typo" {
		t.Errorf("RootCauseName = %q, want CustomName to win", got)
	}
}

func TestNotificationPayload_NoRemediationOptionsLeavesRemediationEmpty(t *testing.T) {
	n := NotificationPayload{ObjectId: "rc-1"}
	if got := n.toTriggerPayload().Remediation; got != "" {
		t.Errorf("Remediation = %q, want empty", got)
	}
}
