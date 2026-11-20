package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
)

const agentMemLimit = 20 << 20

type agentConfig struct {
	Server        string
	ID            string
	DockerSocket  string
	Data          string
	ProxyListen   string // optional: run an embedded Caddy (bench scenario 4 footprint preview)
	ProxyUpstream string
	Token         string // from SD_AGENT_TOKEN; never logged
}

func parseAgentFlags(args []string, stderr io.Writer) (agentConfig, error) {
	var c agentConfig
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.Server, "server", "", "control plane URL, ws://host[:port]/agent/connect")
	fs.StringVar(&c.ID, "id", "", "agent id (letters, digits, _ . -)")
	fs.StringVar(&c.DockerSocket, "docker-socket", dockerapi.DefaultSocket, "Docker socket path")
	fs.StringVar(&c.Data, "data", "/data", "data directory (Caddy storage when the proxy is enabled)")
	fs.StringVar(&c.ProxyListen, "proxy-listen", "", "optional: run an embedded Caddy on this address")
	fs.StringVar(&c.ProxyUpstream, "proxy-upstream", "", "upstream host:port for the optional proxy")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if err := agent.ValidateURL(c.Server); err != nil {
		return c, fmt.Errorf("--server: %w", err)
	}
	if c.ID == "" {
		return c, errors.New("--id is required")
	}
	if (c.ProxyListen == "") != (c.ProxyUpstream == "") {
		return c, errors.New("--proxy-listen and --proxy-upstream go together")
	}
	return c, nil
}

func agentMain(args []string, stderr io.Writer) int {
	cfg, err := parseAgentFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "serverdock agent:", err)
		return 2
	}
	cfg.Token = os.Getenv("SD_AGENT_TOKEN")
	if cfg.Token == "" {
		fmt.Fprintln(stderr, "serverdock agent: SD_AGENT_TOKEN is required")
		return 2
	}
	applyMemLimit(agentMemLimit)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log := slog.New(slog.NewTextHandler(stderr, nil))
	if err := runAgent(ctx, cfg, log); err != nil {
		log.Error("agent stopped", "err", err)
		return 1
	}
	return 0
}

// runAgent dials out to the server and reconnects until ctx is cancelled. The
// agent opens no listening ports itself (the optional proxy is a footprint
// preview only).
func runAgent(ctx context.Context, cfg agentConfig, log *slog.Logger) error {
	if cfg.ProxyListen != "" {
		if err := proxy.Load(proxy.Config{DataDir: cfg.Data, Listen: cfg.ProxyListen, Upstream: cfg.ProxyUpstream}); err != nil {
			return err
		}
		defer proxy.Stop()
	}
	return agent.RunClient(ctx, agent.ClientConfig{
		URL: cfg.Server, Token: cfg.Token, ID: cfg.ID, Log: log,
		Logs: dockerapi.New(cfg.DockerSocket),
	})
}
