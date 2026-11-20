package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestParseAgentFlags(t *testing.T) {
	var buf bytes.Buffer
	c, err := parseAgentFlags([]string{"--server", "ws://127.0.0.1:80/agent/connect", "--id", "n1"}, &buf)
	if err != nil || c.DockerSocket != "/var/run/docker.sock" || c.ID != "n1" {
		t.Fatalf("%+v %v", c, err)
	}
	bad := [][]string{
		{},
		{"--server", "http://x/", "--id", "a"},
		{"--server", "ws://x/agent/connect"},
		{"--server", "ws://x/agent/connect", "--id", "a", "--proxy-listen", ":80"},
		{"--server", "ws://x/agent/connect", "--id", "a", "extra"},
	}
	for _, a := range bad {
		if _, err := parseAgentFlags(a, &buf); err == nil {
			t.Errorf("%v should fail", a)
		}
	}
}

func TestAgentRequiresToken(t *testing.T) {
	t.Setenv("SD_AGENT_TOKEN", "")
	var errb bytes.Buffer
	if code := agentMain([]string{"--server", "ws://127.0.0.1:1/agent/connect", "--id", "a"}, &errb); code != 2 || !strings.Contains(errb.String(), "SD_AGENT_TOKEN") {
		t.Fatalf("code %d %q", code, errb.String())
	}
}

func TestRunAgentStopsOnCancelAndNeverLogsToken(t *testing.T) {
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runAgent(ctx, agentConfig{Server: "ws://127.0.0.1:1/agent/connect", ID: "a", Token: "SUPER-SECRET-TOKEN", DockerSocket: "/nonexistent"},
			slog.New(slog.NewTextHandler(&logs, nil)))
	}()
	time.Sleep(300 * time.Millisecond) // at least one failed dial gets logged
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runAgent did not stop")
	}
	if strings.Contains(logs.String(), "SUPER-SECRET-TOKEN") {
		t.Fatalf("token leaked into logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "will retry") {
		t.Logf("logs: %s", logs.String())
	}
	_ = io.Discard
}
