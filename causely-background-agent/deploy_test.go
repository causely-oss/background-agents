package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/api/scheduledrun"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"
)

// TestKagentManifestsDecodeStrictly decodes every kagent resource under
// deploy/kagent into kagent's own v1alpha3 types, rejecting unknown fields,
// so a misspelled field fails here rather than being dropped by the API
// server's pruning.
func TestKagentManifestsDecodeStrictly(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, file := range []string{"agenttemplate.yaml", "harness.yaml", "model-and-tools.yaml", "egress.yaml"} {
		raw, err := os.ReadFile("deploy/kagent/" + file)
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range bytes.Split(raw, []byte("\n---\n")) {
			var meta struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
			}
			if err := yaml.Unmarshal(doc, &meta); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			var target any
			switch meta.Kind {
			case "Harness":
				target = &v1alpha3.Harness{}
			case "AgentTemplate":
				target = &v1alpha3.AgentTemplate{}
			case "ModelConfig":
				target = &v1alpha3.ModelConfig{}
			case "RemoteMCPServer":
				target = &v1alpha3.RemoteMCPServer{}
			case "Agent":
				target = &v1alpha3.Agent{}
			case "EgressPolicy":
				target = &v1alpha3.EgressPolicy{}
			default:
				continue
			}
			if meta.APIVersion != v1alpha3.GroupVersion.String() {
				t.Fatalf("%s %s: apiVersion %q", file, meta.Kind, meta.APIVersion)
			}
			if err := yaml.UnmarshalStrict(doc, target); err != nil {
				t.Fatalf("%s %s: %v", file, meta.Kind, err)
			}
			seen[meta.Kind] = true

			switch obj := target.(type) {
			case *v1alpha3.Harness:
				if obj.Name != "causely-hermes" || obj.Namespace != "kagent" {
					t.Fatalf("Harness is %s/%s; scheduled-sweep.json, triggering.md and the mediator URL use kagent/causely-hermes", obj.Namespace, obj.Name)
				}
				if obj.Spec.BYO == nil || len(obj.Spec.Workload.Command) == 0 || obj.Spec.Workload.Command[0] != "/usr/local/bin/causely-hermes" {
					t.Fatalf("Harness must be BYO and run the adapter: %+v", obj.Spec)
				}
				env := map[string]string{}
				for _, e := range obj.Spec.Env {
					env[e.Name] = e.Value
				}
				if v := env[envActionMode]; v != actionModeObserve {
					t.Fatalf("the shipped Harness must default to observe mode, got %q", v)
				}
				if v := env[envAllowKubeMutation]; v != "false" {
					t.Fatalf("the shipped Harness must not enable kube mutations, got %q", v)
				}
				// Harness env is literal; the GitHub token is injected at egress.
				if v := env[envGitHubToken]; v != "egress-injected" {
					t.Fatalf("GH_TOKEN must be the egress placeholder, got %q", v)
				}
				if _, ok := env[envKubeconfigData]; ok {
					t.Fatalf("%s would put a credential in the Harness", envKubeconfigData)
				}
			case *v1alpha3.AgentTemplate:
				if obj.Name != "causely-investigator" || obj.Namespace != "kagent" {
					t.Fatalf("AgentTemplate is %s/%s; scheduled-sweep.json, triggering.md and the mediator URL use kagent/causely-investigator", obj.Namespace, obj.Name)
				}
				for _, tool := range obj.Spec.Tools {
					for _, name := range tool.MCP.Tools {
						if strings.Contains(name, "ignore") || strings.HasPrefix(name, "set_") || strings.HasPrefix(name, "create_") ||
							strings.HasPrefix(name, "update_") || strings.HasPrefix(name, "delete_") || name == "submit_feedback" {
							t.Fatalf("write tool %q in the Causely allowlist", name)
						}
					}
				}
			case *v1alpha3.Agent:
				if obj.Name != "causely-investigator" || obj.Spec.HarnessRef.Name != "causely-hermes" || obj.Spec.Template.Ref == nil ||
					obj.Spec.Template.Ref.Name != "causely-investigator" {
					t.Fatalf("Agent must bind causely-investigator to causely-hermes: %+v", obj.Spec)
				}
			}
		}
	}
	for _, kind := range []string{"Harness", "AgentTemplate", "ModelConfig", "RemoteMCPServer", "Agent", "EgressPolicy"} {
		if !seen[kind] {
			t.Fatalf("no %s found in deploy/kagent", kind)
		}
	}
}

// TestScheduledSweepRequest validates deploy/kagent/scheduled-sweep.json the
// way kagent's ScheduledRunService does: strict protojson into
// CreateScheduledRunRequest, then kagent's own schedule normalization and
// next-occurrence check. It also keeps the request consistent with the
// Harness and AgentTemplate it targets and with the adapter's sweep marker.
func TestScheduledSweepRequest(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("deploy/kagent/scheduled-sweep.json")
	if err != nil {
		t.Fatal(err)
	}
	var request apiv1.CreateScheduledRunRequest
	if err := protojson.Unmarshal(raw, &request); err != nil {
		t.Fatalf("scheduled-sweep.json: %v", err)
	}
	config := scheduledrun.Normalize(request.GetConfig())
	if _, err := scheduledrun.Next(config, time.Now()); err != nil {
		t.Fatalf("schedule rejected by kagent: %v", err)
	}
	if request.GetAgent().GetName() != "causely-investigator" || request.GetAgent().GetNamespace() != "kagent" {
		t.Fatalf("ScheduledRun targets %v, not the shipped Agent", request.GetAgent())
	}
	if !isSweepPrompt(config.GetPrompt()) {
		t.Fatalf("sweep prompt must start with %q", sweepMarker)
	}
	// kagent cancels the execution at its deadline; the adapter's own sweep
	// limit should fire first so Hermes is cancelled cleanly.
	if got := config.GetExecutionTimeout().AsDuration(); got <= defaultMaxSweepDuration {
		t.Fatalf("executionTimeout %s should exceed the default sweep limit %s", got, defaultMaxSweepDuration)
	}
}
