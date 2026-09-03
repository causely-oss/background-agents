package main

// NotificationPayload mirrors the JSON shape Causely's mediator already sends
// to any configured notification destination — Slack, Teams, or (the one this
// agent consumes) a "causelybot"/"generic" webhook destination. This is
// Causely's existing, already-shipped notification contract (see
// Causely/causely's pkg/types.NotificationPayload), not something specific to
// this agent — wiring a trigger is purely a mediator-side notification-config
// change (destination type "causelybot", URL, and a Bearer token), no code
// change to Causely's mediator required.
type NotificationPayload struct {
	Labels      map[string]string   `json:"labels,omitempty"`
	Entity      NotificationEntity  `json:"entity"`
	Name        string              `json:"name"`
	CustomName  string              `json:"customName,omitempty"`
	Severity    string              `json:"severity"`
	ObjectId    string              `json:"objectId"`
	Description NotificationProblem `json:"description"`
}

type NotificationEntity struct {
	Id   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type NotificationProblem struct {
	Summary            string                    `json:"summary,omitempty"`
	RemediationOptions []NotificationRemediation `json:"remediationOptions,omitempty"`
}

type NotificationRemediation struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// causelyNamespaceLabel / causelyGitHubRepoLabel are the entity label keys
// used for scope hints. causely.ai/namespace already exists on every entity
// today; causely.ai/github-repo is an opt-in label a customer would add to
// their own workload manifests to route root causes to the right repo.
const (
	causelyNamespaceLabel  = "causely.ai/namespace"
	causelyGitHubRepoLabel = "causely.ai/github-repo"
)

// toTriggerPayload adapts Causely's real notification payload to this agent's
// internal TriggerPayload. RootCauseName prefers CustomName (the LLM-generated
// title) when present, falling back to Name (the underlying CML root cause
// type), matching how Causely's own Slack/Teams rendering already does this.
//
// Note: SlackChannel/SlackThreadTS are deliberately left empty here — those
// only exist after mediator has already posted to Slack in its Slack-specific
// code path, which is a separate delivery from this generic notification
// dispatch. A Slack reply in "act" mode therefore can't be threaded to the
// original alert via this trigger source; it would need a fixed target
// channel configured on this agent instead, not yet implemented.
func (p NotificationPayload) toTriggerPayload() TriggerPayload {
	remediation := ""
	if len(p.Description.RemediationOptions) > 0 {
		remediation = p.Description.RemediationOptions[0].Description
	}
	name := p.Name
	if p.CustomName != "" {
		name = p.CustomName
	}
	return TriggerPayload{
		RootCauseID:     p.ObjectId,
		EntityID:        p.Entity.Id,
		EntityName:      p.Entity.Name,
		RootCauseName:   name,
		Severity:        p.Severity,
		Description:     p.Description.Summary,
		Remediation:     remediation,
		EntityNamespace: p.Labels[causelyNamespaceLabel],
		GitHubRepoLabel: p.Labels[causelyGitHubRepoLabel],
	}
}
