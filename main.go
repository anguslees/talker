package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/anguslees/talker/internal/config"
	"github.com/anguslees/talker/internal/hermes"
	"github.com/anguslees/talker/internal/live"
	"github.com/anguslees/talker/internal/orchestrator"
	"github.com/anguslees/talker/internal/server"
	"github.com/anguslees/talker/internal/tasks"
	"github.com/anguslees/talker/internal/web"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("Talker stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnvironment()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.APIKey, err = resolveKey("GEMINI_API_KEY", cfg.APIKey, cfg.APIKeyFile); err != nil {
		return err
	}
	if cfg.HermesKey, err = resolveKey("HERMES_API_KEY", cfg.HermesKey, cfg.HermesKeyFile); err != nil {
		return err
	}
	client, err := hermes.New(cfg.HermesURL, cfg.HermesKey)
	if err != nil {
		return err
	}
	if err := verifyHermes(ctx, client); err != nil {
		return err
	}
	manager, err := tasks.Open(ctx, client, cfg.StatePath)
	if err != nil {
		return err
	}
	defer manager.Close()
	agents, err := orchestrator.New(manager)
	if err != nil {
		return fmt.Errorf("initialize ADK tools: %w", err)
	}
	voice := live.Config{Model: cfg.Model, Voice: cfg.Voice, Language: cfg.Language, Temperature: float32(cfg.Temperature), QuietPeriod: cfg.QuietPeriod, Instruction: cfg.Instruction, SpeechStart: cfg.SpeechStart, SpeechPrefix: cfg.SpeechPrefix}
	broker, err := live.NewBroker(voice, cfg.APIKey, agents.Declarations())
	if err != nil {
		return err
	}
	if cfg.Check {
		result, err := broker.Check(ctx)
		if err != nil {
			return err
		}
		slog.Info("Gemini Live check passed", "model", result.Model, "setup", result.SetupTime.Round(time.Millisecond), "first_audio", result.FirstAudio.Round(time.Millisecond), "audio_bytes", result.AudioBytes)
		return nil
	}
	handler := server.New(server.Config{Model: cfg.Model, Voice: cfg.Voice, EventToken: cfg.EventToken, Tasks: manager, Web: web.Handler(), Control: live.NewControl(ctx, manager, agents), Token: broker})
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	slog.Info("Talker listening", "url", "http://"+listener.Addr().String(), "model", cfg.Model, "audio", "browser directly to Gemini")
	if cfg.APIKey == "" {
		slog.Warn("Voice needs GEMINI_API_KEY or GEMINI_API_KEY_FILE; the local console is available")
	}
	err = srv.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// resolveKey reads a secret from a file when configured, so it never appears in
// argv or shell history. Exactly one of the two sources may be set.
func resolveKey(name, value, path string) (string, error) {
	if path == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("set either %s or %s_FILE, not both", name, name)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s_FILE: %w", name, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return "", fmt.Errorf("%s_FILE must contain a single key of at most 4096 bytes", name)
	}
	return strings.TrimSpace(string(data)), nil
}

// verifyHermes proves the credential at startup. A rejected key is a hard error,
// because every tool would otherwise fail with an opaque 401 at speech time; an
// unreachable gateway is only a warning, since a port-forward may come up later.
func verifyHermes(ctx context.Context, client *hermes.Client) error {
	if !client.Configured() {
		slog.Warn("Hermes not configured; task tools will report that Hermes is unavailable")
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	caps, err := client.Capabilities(checkCtx)
	var httpErr *hermes.HTTPError
	switch {
	case err == nil:
		slog.Info("Hermes connected", "platform", caps.Platform, "model", caps.Model, "durable_runs", caps.Features.RunsIdempotency.Durable)
		return nil
	case errors.As(err, &httpErr) && (httpErr.StatusCode == 401 || httpErr.StatusCode == 403):
		return fmt.Errorf("Hermes rejected the API key (%s). HERMES_API_KEY must equal the gateway's API_SERVER_KEY, found in the Hermes .env or the running gateway's environment", httpErr.Message)
	case errors.As(err, &httpErr):
		return fmt.Errorf("Hermes at the configured URL did not behave like the API Server adapter (%v). Use the :8642 API adapter, not the :9119 dashboard", err)
	default:
		slog.Warn("Hermes unreachable at startup; tasks will fail until it is", "error", err)
		return nil
	}
}
