package dockerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// Event is a container event, reduced to what the reconciler uses. Text from
// Docker is cleaned.
type Event struct {
	Action   string // "die", "start", "stop", "destroy", ...
	ID       string // full container id
	Service  string // serverdock.service label ("" if absent)
	Deploy   string // serverdock.deploy label
	ExitCode string // exitCode attribute on die
	Time     int64  // unix seconds
}

// Events streams container events whose labels match (each "key" or
// "key=value"; all must match) into fn until ctx is cancelled, Docker ends the
// stream (nil) or fn returns an error. A Docker line over MaxJSONLine is
// ErrLineTooLong: the caller resyncs. fn runs on the caller's goroutine. The
// stream has no read deadline: it is idle between events by design, and the
// caller owns the reconnect.
func (c *Client) Events(ctx context.Context, labels []string, fn func(Event) error) error {
	return c.EventsOpen(ctx, labels, nil, fn)
}

// EventsOpen is Events with onOpen (may be nil), called once when Docker's
// response headers have arrived: from then on no event can be missed, so a
// caller can list state knowing everything after the list is in the stream.
func (c *Client) EventsOpen(ctx context.Context, labels []string, onOpen func(), fn func(Event) error) error {
	// Only the actions the reconciler reads: this leaves out the exec_* traffic
	// of container health checks.
	f := map[string][]string{
		"type":  {"container"},
		"event": {"start", "stop", "die", "oom", "destroy", "restart", "kill"},
	}
	for _, l := range labels {
		if l == "" || len(l) > 256 || strings.ContainsAny(l, "\x00\r\n") {
			return errors.New("dockerapi: invalid label filter")
		}
		f["label"] = append(f["label"], l)
	}
	fj, _ := json.Marshal(f)
	resp, err := c.stream(ctx, http.MethodGet, "/events?filters="+url.QueryEscape(string(fj)), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if onOpen != nil {
		onOpen()
	}
	lines := newJSONLines(resp.Body)
	for {
		line, err := lines.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("dockerapi: events: %w", err)
		}
		var m struct {
			Action string
			Type   string
			Time   int64
			Actor  struct {
				ID         string
				Attributes map[string]string
			}
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("dockerapi: events: bad line: %w", err)
		}
		if m.Type != "container" || !ValidFullID(m.Actor.ID) {
			continue
		}
		ev := Event{
			Action:   agent.CleanText(m.Action, 64),
			ID:       m.Actor.ID,
			Service:  agent.CleanText(m.Actor.Attributes["serverdock.service"], 64),
			Deploy:   agent.CleanText(m.Actor.Attributes["serverdock.deploy"], 64),
			ExitCode: agent.CleanText(m.Actor.Attributes["exitCode"], 16),
			Time:     m.Time,
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
}
