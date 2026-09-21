package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"go.uber.org/zap"
)

// TriggerPayload is this agent's internal representation of one investigation
// request, regardless of where it came from — POST /trigger (adapted from
// Causely's real NotificationPayload, see notification_payload.go), the poll
// loop (see poll.go), or a Slack "Fix it" button (see slack_actions.go).
type TriggerPayload struct {
	IssueID       string `json:"issue_id"`
	EntityID      string `json:"entity_id"`
	EntityName    string `json:"entity_name"`
	DiagnosisName string `json:"diagnosis_name"`
	Severity      string `json:"severity"`
	Description   string `json:"description"`
	Remediation   string `json:"remediation"`
	SlackChannel  string `json:"slack_channel"`
	SlackThreadTS string `json:"slack_thread_ts"`

	// Scope hints from the Issue's entity labels, used by inScope() in
	// scope.go to decide whether this agent instance should act on this
	// Issue at all.
	EntityNamespace string `json:"entity_namespace"`
	GitHubRepoLabel string `json:"github_repo_label"`
}

// Run starts the agent's HTTP server (and poll loop, if configured) and
// blocks until it receives SIGINT/SIGTERM. It's the sole entrypoint called by
// cmd/causely-background-agent's main().
func Run() {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()
	shutdownTelemetry, err := initTelemetry(context.Background())
	if err != nil {
		logger.Fatal("initialize telemetry", zap.Error(err))
	}
	defer func() { _ = shutdownTelemetry(context.Background()) }()

	var configPath string
	flag.StringVar(&configPath, "config", "/config/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := loadConfig(configPath)
	if err != nil {
		logger.Fatal("invalid config", zap.Error(err))
	}
	logger.Info("causely-background-agent scope",
		zap.String("github_repo", cfg.GitHubRepo),
		zap.Strings("scope_namespaces", cfg.ScopeNamespaces))
	logger.Info("causely-background-agent cost caps",
		zap.Float64("max_cost_usd_per_incident", cfg.MaxCostUSD),
		zap.Float64("max_weekly_cost_usd", cfg.MaxWeeklyCostUSD),
		zap.String("cost_state_file", cfg.CostStateFile))
	logger.Info("causely-background-agent action mode", zap.String("action_mode", cfg.ActionMode))
	for _, s := range cfg.MCPServers {
		logger.Info("mcp server configured",
			zap.String("name", s.Name), zap.String("url", s.URL), zap.Bool("has_token", s.Token != ""))
	}
	// loadConfig already refused to start with an unset TRIGGER_SHARED_SECRET /
	// SLACK_SIGNING_SECRET unless the operator explicitly opted in via
	// allow_unauthenticated_trigger / allow_unauthenticated_slack_actions —
	// these warnings only fire in that deliberate-opt-in case.
	if cfg.TriggerSharedSecret == "" {
		logger.Warn("allow_unauthenticated_trigger is set — POST /trigger accepts unauthenticated requests from anyone who can reach this service")
	}
	if cfg.SlackSigningSecret == "" {
		logger.Warn("allow_unauthenticated_slack_actions is set — POST /slack/actions accepts unverified requests from anyone who can reach this service")
	}

	weekly := newWeeklyBudget(cfg.MaxWeeklyCostUSD, cfg.CostStateFile).withConcurrencyLimit(cfg.MaxConcurrentInvestigations)
	rec := newRecorder(cfg.InvestigationRecordPath, logger)
	kc, kcErr := newKubeClient()
	logKubeClientInit(logger, kc, kcErr)

	if cfg.Poll.Enabled {
		go runPollLoop(logger, cfg, weekly, rec, kc)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /slack/actions", handleSlackAction(logger, cfg, weekly, rec, kc))

	mux.HandleFunc("POST /trigger", func(w http.ResponseWriter, r *http.Request) {
		if !validTriggerAuth(r, cfg.TriggerSharedSecret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		var notification NotificationPayload
		if err := json.Unmarshal(body, &notification); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		payload := notification.toTriggerPayload()
		if payload.IssueID == "" {
			http.Error(w, "objectId required", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		go runAgent(logger, cfg, payload, weekly, rec, kc, triggerSourceWebhook)
	})

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: mux}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("causely-background-agent listening", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	<-stop
	logger.Info("shutting down")
	_ = srv.Shutdown(context.Background())
}

// validTriggerAuth checks the Authorization: Bearer <secret> header against
// TRIGGER_SHARED_SECRET. If no secret is configured, every request is allowed
// (with a startup warning already logged) — this agent is a standalone
// network-reachable service, not an internal same-repo call, so it can't lean
// on network topology alone the way the old design implicitly did.
func validTriggerAuth(r *http.Request, secret string) bool {
	if secret == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}
