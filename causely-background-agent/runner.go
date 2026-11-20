package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	harnessruntime "github.com/kagent-dev/kagent/go/harness/runtime"
)

type hermesRunner struct {
	logger    *slog.Logger
	command   []string
	dataDir   string
	workspace string
	// env is the complete environment for the Hermes process. It is built
	// once at startup (see processEnvironment) so bootstrap-only secrets such
	// as KUBECONFIG_DATA never reach Hermes or its terminal tool.
	env []string
	// turnLimit bounds one A2A turn (one investigation, or one sweep) given
	// its prompt. Zero means no limit. Hermes has no cost cap, only the
	// agent.max_turns iteration limit, so this is the main spend bound.
	turnLimit func(prompt string) time.Duration
	// model is the configured model, for attributing token usage.
	model hermesModel
	// usage converts Hermes' session-cumulative token counts into per-turn
	// counts for the turn span.
	usage usageMeter

	mu         sync.Mutex
	process    *exec.Cmd
	input      io.WriteCloser
	connection *acp.ClientSideConnection
	client     *hermesACPClient
	sessionID  string
}

func newHermesRunner(logger *slog.Logger, command []string, dataDir, workspace string, env []string, turnLimit func(string) time.Duration) *hermesRunner {
	if turnLimit == nil {
		turnLimit = func(string) time.Duration { return 0 }
	}
	return &hermesRunner{logger: logger, command: command, dataDir: dataDir, workspace: workspace, env: env, turnLimit: turnLimit}
}

func (r *hermesRunner) Check(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensureConnected(ctx, "")
}

func (r *hermesRunner) Run(ctx context.Context, turn harnessruntime.Turn, sink harnessruntime.EventSink) (harnessruntime.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureConnected(ctx, turn.ContinuationID); err != nil {
		return harnessruntime.Outcome{}, err
	}
	if turn.ContinuationID == "" {
		if err := sink.SessionStarted(harnessruntime.SessionStarted{ContinuationID: r.sessionID}); err != nil {
			return harnessruntime.Outcome{}, err
		}
	}

	r.client.setSink(sink)
	defer r.client.setSink(nil)
	response, err := r.prompt(ctx, turn.Prompt)
	reportTurnUsage(ctx, sink, r.model, r.usage.turn(response.Usage))
	return harnessruntime.Outcome{}, err
}

func (r *hermesRunner) prompt(ctx context.Context, prompt string) (acp.PromptResponse, error) {
	promptCtx := ctx
	limit := r.turnLimit(prompt)
	if limit > 0 {
		var cancel context.CancelFunc
		promptCtx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	// On context expiry the ACP SDK sends session/cancel, so Hermes stops
	// calling the model instead of running on unobserved.
	response, err := r.connection.Prompt(promptCtx, acp.PromptRequest{
		SessionId: acp.SessionId(r.sessionID),
		Prompt:    []acp.ContentBlock{acp.TextBlock(prompt)},
	})
	if err != nil {
		if ctx.Err() == nil && errors.Is(promptCtx.Err(), context.DeadlineExceeded) {
			return response, fmt.Errorf("Hermes turn exceeded %s (%s / %s) and was cancelled: %w", limit, envMaxTurnDuration, envMaxSweepDuration, context.DeadlineExceeded)
		}
		return response, fmt.Errorf("prompt Hermes session %s: %w", r.sessionID, err)
	}
	return response, nil
}

func (r *hermesRunner) ensureConnected(ctx context.Context, continuationID string) error {
	if r.connection != nil {
		select {
		case <-r.connection.Done():
			r.stopProcess()
		default:
			if continuationID == "" || continuationID == r.sessionID {
				return nil
			}
			return fmt.Errorf("Hermes runtime is bound to session %s, not %s", r.sessionID, continuationID)
		}
	}
	if len(r.command) == 0 {
		return fmt.Errorf("Hermes command is required")
	}

	cmd := exec.Command(r.command[0], r.command[1:]...)
	cmd.Dir = r.workspace
	cmd.Env = append(append([]string(nil), r.env...), "HERMES_HOME="+r.dataDir+"/hermes", "HOME="+r.dataDir)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open Hermes stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open Hermes stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("start Hermes ACP process: %w", err)
	}

	client := newHermesACPClient()
	connection := acp.NewClientSideConnection(client, stdin, stdout)
	connection.SetLogger(r.logger)
	if _, err := connection.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion:    acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{},
	}); err != nil {
		stopCommand(cmd, stdin)
		return fmt.Errorf("initialize Hermes ACP connection: %w", err)
	}

	if continuationID == "" {
		session, err := connection.NewSession(ctx, acp.NewSessionRequest{Cwd: r.workspace, McpServers: []acp.McpServer{}})
		if err != nil {
			stopCommand(cmd, stdin)
			return fmt.Errorf("create Hermes ACP session: %w", err)
		}
		r.sessionID = string(session.SessionId)
	} else {
		if _, err := connection.LoadSession(ctx, acp.LoadSessionRequest{
			Cwd: r.workspace, SessionId: acp.SessionId(continuationID), McpServers: []acp.McpServer{},
		}); err != nil {
			stopCommand(cmd, stdin)
			return fmt.Errorf("load Hermes ACP session %s: %w", continuationID, err)
		}
		r.sessionID = continuationID
	}

	r.usage.reset()
	r.process = cmd
	r.input = stdin
	r.connection = connection
	r.client = client
	return nil
}

func (r *hermesRunner) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopProcess()
}

func (r *hermesRunner) stopProcess() error {
	var err error
	if r.input != nil {
		err = r.input.Close()
	}
	if r.process != nil && r.process.Process != nil {
		if killErr := r.process.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) && err == nil {
			err = killErr
		}
		_ = r.process.Wait()
	}
	r.process = nil
	r.input = nil
	r.connection = nil
	r.client = nil
	r.sessionID = ""
	return err
}

func stopCommand(cmd *exec.Cmd, input io.Closer) {
	_ = input.Close()
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
