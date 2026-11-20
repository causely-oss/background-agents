// Command causely-hermes is a kagent BYO Harness runtime: it serves A2A on
// the Actor's private port and drives Hermes Agent over ACP (stdio). All
// investigation behavior lives in the AgentTemplate system prompt and the
// Hermes skills baked into the image; this binary only translates kagent
// configuration into Hermes configuration and bridges the two protocols.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/kagent-dev/kagent/go/adk/pkg/app"
	"github.com/kagent-dev/kagent/go/adk/pkg/bootstrap"
	contract "github.com/kagent-dev/kagent/go/api/bootstrap"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

const (
	// configEnv and agentCardEnv carry the configuration directly, for
	// running the image outside kagent (--check, local tests). In an Actor
	// kagent provides KAGENT_BOOTSTRAP_JSON instead and the adapter fetches
	// the configuration from the control plane.
	configEnv    = "KAGENT_CONFIG_JSON"
	agentCardEnv = "KAGENT_AGENT_CARD_JSON"
	dataDir      = "/data"
	// workspaceDir is Hermes' terminal and session cwd. Not /data/workspace:
	// Substrate mounts /data as a fresh tmpfs with a root-owned
	// /data/workspace the adapter's user cannot write to.
	workspaceDir = "/data/work"
	skillsDir    = "/opt/causely/skills"
	// secretsDir holds credential files (gh token, kubeconfig). It is
	// deliberately outside /data, which Substrate snapshots, and must match
	// the path in gh-wrapper.sh.
	secretsDir  = "/tmp/causely-secrets"
	privatePort = "80"
)

func main() {
	check := flag.Bool("check", false, "validate configuration and Hermes ACP startup, then exit")
	flag.Parse()
	logger, err := logging.New(os.Stderr, "info")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := logging.IntoContext(context.Background(), logger)
	if err := run(ctx, *check); err != nil {
		logger.ErrorContext(ctx, "Causely Hermes Harness stopped", "error", err)
		os.Exit(1)
	}
}

// loadConfiguration returns the compiled configuration and Agent Card: from
// kagent's bootstrap bundle in an Actor, or from configEnv/agentCardEnv.
func loadConfiguration(ctx context.Context) (configJSON, agentCardJSON []byte, err error) {
	if strings.TrimSpace(os.Getenv(contract.Environment)) != "" {
		bundle, err := bootstrap.FromEnvironment(ctx, os.Getenv)
		if err != nil {
			return nil, nil, fmt.Errorf("load kagent bootstrap: %w", err)
		}
		return bundle.ConfigJson, bundle.AgentCardJson, nil
	}
	if configJSON, err = requiredEnvironment(configEnv); err != nil {
		return nil, nil, fmt.Errorf("%w (or %s from kagent)", err, contract.Environment)
	}
	if agentCardJSON, err = requiredEnvironment(agentCardEnv); err != nil {
		return nil, nil, err
	}
	return configJSON, agentCardJSON, nil
}

func run(ctx context.Context, check bool) error {
	configJSON, agentCardJSON, err := loadConfiguration(ctx)
	if err != nil {
		return err
	}
	var card a2atype.AgentCard
	if err := json.Unmarshal(agentCardJSON, &card); err != nil {
		return fmt.Errorf("decode Agent Card: %w", err)
	}
	if strings.TrimSpace(card.Name) == "" {
		return fmt.Errorf("Agent Card name is required")
	}
	config, err := decodeAgentConfig(configJSON)
	if err != nil {
		return err
	}
	runtimeCfg, input := config.GetRuntime(), config.GetAgent()
	logger, err := logging.New(os.Stderr, cmp.Or(runtimeCfg.GetLogLevel(), "info"))
	if err != nil {
		return err
	}
	ctx = logging.IntoContext(ctx, logger)

	pol, err := loadPolicy(os.Getenv)
	if err != nil {
		return err
	}
	environ, env, err := prepareProcessEnvironment(os.Environ(), secretsDir, headerSecretNames(input), fileExists)
	if err != nil {
		return err
	}

	providers, err := initTelemetry(ctx, card.Name)
	if err != nil {
		logger.ErrorContext(ctx, "failed to initialize telemetry", "error", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := providers.Shutdown(shutdownCtx); err != nil {
			logger.ErrorContext(ctx, "failed to shut down telemetry", "error", err)
		}
	}()

	l := layout{DataDir: dataDir, Workspace: workspaceDir}
	if fileExists(skillsDir) {
		l.SkillsDir = skillsDir
	}
	hermesCfg, err := materializeHermesConfig(input, pol, env, l)
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "Hermes configuration written",
		"action_mode", pol.ActionMode, "kube_mutations", pol.AllowKubeMutations,
		"github_repo", pol.GitHubRepo, "kubectl", env.Kubectl, "github_token", env.GitHubToken,
		"tracing", providers.TracesEnabled())

	runner := newHermesRunner(logger, []string{"hermes", "acp"}, dataDir, workspaceDir, environ, pol.turnLimit)
	runner.model = hermesCfg.Model
	defer runner.Close()
	if check {
		if err := runner.Check(ctx); err != nil {
			return fmt.Errorf("check Hermes runtime: %w", err)
		}
		return nil
	}
	store, err := continuation.New(dataDir+"/adapter", "hermes", validateSessionID)
	if err != nil {
		return fmt.Errorf("create continuation store: %w", err)
	}
	telemetry := runtimeTelemetry(hermesCfg.Model, os.Environ())
	executor, err := runtimea2a.New(runner, store, telemetry)
	if err != nil {
		return fmt.Errorf("create A2A executor: %w", err)
	}
	application, err := app.New(app.AppConfig{
		AgentCard: card, Port: privatePort, AppName: card.Name,
		ControlPlaneURL: runtimeCfg.GetControlPlaneUrl(), MaxContentLength: runtimeCfg.MaxContentLength,
		Logger: logger, Telemetry: telemetry, Flush: providers.ForceFlush,
		HandlerOpts: []a2asrv.RequestHandlerOption{a2asrv.WithExecutorContextInterceptor(startMetrics(providers.StartMetrics))},
	}, executor)
	if err != nil {
		return fmt.Errorf("construct private A2A app: %w", err)
	}
	return application.Run()
}

// startMetrics starts metrics on the first request, which runs after the
// Substrate snapshot restore (kagent's harnesses do the same).
type startMetrics func()

func (start startMetrics) Intercept(ctx context.Context, _ *a2asrv.ExecutorContext) (context.Context, error) {
	start()
	return ctx, nil
}

func requiredEnvironment(name string) ([]byte, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	return []byte(value), nil
}

func validateSessionID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("Hermes session ID is empty")
	}
	return nil
}
