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
	fp := "sha256:" + strings.Repeat("ab", 32)
	c, err = parseAgentFlags([]string{"--server", "wss://hub.example:7443/agent/connect", "--ca-fingerprint", fp, "--id", "n1"}, &buf)
	if err != nil || c.CAFingerprint != fp {
		t.Fatalf("wss: %+v %v", c, err)
	}
	bad := [][]string{
		{},
		{"--server", "http://x/", "--id", "a"},
		{"--server", "ws://127.0.0.1/agent/connect"},
		{"--server", "ws://127.0.0.1/agent/connect", "--id", "a", "--proxy-listen", ":80"},
		{"--server", "ws://127.0.0.1/agent/connect", "--id", "a", "extra"},
		{"--server", "ws://hub.example/agent/connect", "--id", "a"},                           // cleartext to a remote host
		{"--server", "wss://hub.example/agent/connect", "--id", "a"},                          // no fingerprint
		{"--server", "wss://hub.example/agent/connect", "--id", "a", "--ca-fingerprint", "x"}, // bad fingerprint
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

func TestAgentRefusesWeakToken(t *testing.T) {
	t.Setenv("SD_AGENT_TOKEN", "too-short-to-be-safe")
	var errb bytes.Buffer
	if code := agentMain([]string{"--server", "ws://127.0.0.1:1/agent/connect", "--id", "a"}, &errb); code != 2 || !strings.Contains(errb.String(), "too short") {
		t.Fatalf("code %d %q", code, errb.String())
	}
	if strings.Contains(errb.String(), "too-short-to-be-safe") {
		t.Error("token echoed")
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
