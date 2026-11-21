package agentops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// stub is a Caller that answers every op with a fixed result and optional lines.
type stub struct {
	result json.RawMessage
	lines  []agent.Line
	err    error
	calls  int
}

func (s *stub) Call(_ context.Context, _ string, _ any, onLine func(agent.Line)) (json.RawMessage, error) {
	s.calls++
	for _, l := range s.lines {
		if onLine != nil {
			onLine(l)
		}
	}
	return s.result, s.err
}

var id64 = strings.Repeat("a", 64)

func TestClientRefusesAMalformedReply(t *testing.T) {
	ctx := context.Background()
	good := map[string]struct {
		reply string
		call  func(c *Client) error
	}{
		"pull":  {`{"image_id":"` + imgID + `"}`, func(c *Client) error { _, err := c.ImagePull(ctx, "x:1", 0, nil); return err }},
		"image": {`{"id":"` + imgID + `","repo_digests":[],"exposed_ports":[],"volumes":[],"reserved_label_keys":[]}`, func(c *Client) error { _, err := c.ImageInspect(ctx, "x:1", false); return err }},
		"create": {`{"id":"` + id64 + `","existed":false}`, func(c *Client) error {
			_, err := c.ContainerCreate(ctx, "n", goodCreate().Spec, "10.250.7.2", imgID)
			return err
		}},
		"start":   {`{}`, func(c *Client) error { return c.ContainerStart(ctx, id64) }},
		"stop":    {`{}`, func(c *Client) error { return c.ContainerStop(ctx, id64, 1) }},
		"restart": {`{}`, func(c *Client) error { return c.ContainerRestart(ctx, id64, 1) }},
		"remove":  {`{}`, func(c *Client) error { return c.ContainerRemove(ctx, id64) }},
		"inspect": {`{"id":"` + id64 + `","state":"running","labels":{}}`, func(c *Client) error { _, err := c.ContainerInspect(ctx, id64); return err }},
		"list":    {`{"containers":[]}`, func(c *Client) error { _, err := c.ContainerList(ctx); return err }},
		"ports":   {`{"containers":[]}`, func(c *Client) error { _, err := c.ContainerPorts(ctx); return err }},
		"ensure":  {`{"id":"n","subnet":"10.250.7.0/24","gateway":"10.250.7.1"}`, func(c *Client) error { _, err := c.NetworkEnsure(ctx, "n", "10.250.7.0/24"); return err }},
		"netinsp": {`{"subnet":"10.250.7.0/24","gateway":"10.250.7.1","ip_range":"10.250.7.192/26","used":[]}`, func(c *Client) error { _, err := c.NetworkInspect(ctx); return err }},
		"health": {`{"ok":true,"ms":1}`, func(c *Client) error {
			_, err := c.HealthCheck(ctx, HealthArgs{Target: "x", Kind: KindTCP})
			return err
		}},
		"events": {`{}`, func(c *Client) error { return c.Events(ctx, func(Event) error { return nil }) }},
	}
	for name, tc := range good {
		s := &stub{result: json.RawMessage(tc.reply)}
		if err := tc.call(NewClient(s)); err != nil {
			t.Errorf("%s: a good reply failed: %v", name, err)
			continue
		}
		// An unknown key, a duplicate key and a non-object are refused.
		unknown := strings.Replace(tc.reply, "{", `{"extra":1,`, 1)
		if tc.reply == "{}" {
			unknown = `{"extra":1}`
		}
		bads := map[string]string{"unknown key": unknown, "not an object": `[]`}
		if d := dupFirstKey(t, tc.reply); d != "" {
			bads["duplicate key"] = d
		}
		for why, bad := range bads {
			if err := tc.call(NewClient(&stub{result: json.RawMessage(bad)})); !errors.Is(err, ErrBadReply) {
				t.Errorf("%s / %s: %v", name, why, err)
			}
		}
	}
}

func dupFirstKey(t *testing.T, reply string) string {
	t.Helper()
	i := strings.Index(reply, `"`)
	if i < 0 {
		return ""
	}
	j := strings.Index(reply[i+1:], `"`) + i + 1
	k := j + 1 // after the key and its colon
	if reply[k] != ':' {
		return ""
	}
	end := strings.IndexAny(reply[k:], ",}") + k
	return reply[:i] + reply[i:end] + "," + reply[i:end] + reply[end:]
}

func TestClientRefusesImplausibleValues(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		reply string
		call  func(c *Client) error
	}{
		"pull: image id is a tag": {`{"image_id":"nginx:1"}`, func(c *Client) error { _, err := c.ImagePull(ctx, "x:1", 0, nil); return err }},
		"create: short id": {`{"id":"abc","existed":false}`, func(c *Client) error {
			_, err := c.ContainerCreate(ctx, "n", goodCreate().Spec, "10.250.7.2", imgID)
			return err
		}},
		"inspect: another id":     {`{"id":"` + strings.Repeat("b", 64) + `","state":"running","labels":{}}`, func(c *Client) error { _, err := c.ContainerInspect(ctx, id64); return err }},
		"inspect: odd state":      {`{"id":"` + id64 + `","state":"RUNNING\u001b","labels":{}}`, func(c *Client) error { _, err := c.ContainerInspect(ctx, id64); return err }},
		"inspect: address":        {`{"id":"` + id64 + `","state":"running","ip":"10.250.7.002","labels":{}}`, func(c *Client) error { _, err := c.ContainerInspect(ctx, id64); return err }},
		"list: short id":          {`{"containers":[{"id":"x","name":"n","image_id":"","state":"","labels":{"project":"","service":"","deploy":"","attempt":""}}]}`, func(c *Client) error { _, err := c.ContainerList(ctx); return err }},
		"netinsp: bad address":    {`{"subnet":"10.250.7.0/24","gateway":"10.250.7.1","ip_range":"","used":["nope"]}`, func(c *Client) error { _, err := c.NetworkInspect(ctx); return err }},
		"netinsp: no gateway":     {`{"subnet":"10.250.7.0/24","gateway":"","ip_range":"","used":[]}`, func(c *Client) error { _, err := c.NetworkInspect(ctx); return err }},
		"ensure: no gateway":      {`{"id":"n","subnet":"10.250.7.0/24","gateway":""}`, func(c *Client) error { _, err := c.NetworkEnsure(ctx, "n", "10.250.7.0/24"); return err }},
		"health: negative ms":     {`{"ok":true,"ms":-1}`, func(c *Client) error { _, err := c.HealthCheck(ctx, HealthArgs{}); return err }},
		"image: too many digests": {`{"id":"` + imgID + `","repo_digests":["a","b","c","d","e","f","g","h","i"],"exposed_ports":[],"volumes":[],"reserved_label_keys":[]}`, func(c *Client) error { _, err := c.ImageInspect(ctx, "x:1", false); return err }},
		"ports: too many ports":   {`{"containers":[{"name":"x","ports":[{},{},{},{},{}]}]}`, func(c *Client) error { _, err := c.ContainerPorts(ctx); return err }},
	} {
		if err := tc.call(NewClient(&stub{result: json.RawMessage(tc.reply)})); !errors.Is(err, ErrBadReply) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestClientCleansTextFromTheAgent(t *testing.T) {
	ctx := context.Background()
	dirty := "a\u001b[31mb‮c\x00"
	// progress
	s := &stub{result: json.RawMessage(`{"image_id":"` + imgID + `","repo_digest":"x@sha256:\u001b[2J"}`),
		lines: []agent.Line{{T: agent.LineProgress, P: json.RawMessage(`{"layers":1,"layers_done":0,"bytes":1,"bytes_total":2,"status":"Down\u001b[2Jloading‮"}`)},
			{T: agent.LineProgress, P: json.RawMessage(`{"layers":1,"extra":1}`)}, // dropped
		}}
	var got []PullProgress
	res, err := NewClient(s).ImagePull(ctx, "x:1", 0, func(p PullProgress) { got = append(got, p) })
	if err != nil || len(got) != 1 || got[0].Status != "Down[2Jloading" || strings.ContainsAny(res.RepoDigest, "\x1b") {
		t.Fatalf("%+v %+v %v", res, got, err)
	}
	// list, inspect, ports, health
	b, _ := json.Marshal(map[string]any{"containers": []map[string]any{{"id": id64, "name": dirty, "image_id": "", "state": "ru\u001bn",
		"labels": map[string]string{"project": dirty, "service": dirty, "deploy": dirty, "attempt": dirty}}}})
	list, err := NewClient(&stub{result: b}).ContainerList(ctx)
	if err != nil || strings.ContainsAny(list.Containers[0].Name+list.Containers[0].State+list.Containers[0].Labels.Project, "\x1b\x00‮") {
		t.Fatalf("%+v %v", list, err)
	}
	b, _ = json.Marshal(map[string]any{"id": id64, "state": "running", "labels": map[string]string{dirty: dirty}, "started_at": dirty})
	st, err := NewClient(&stub{result: b}).ContainerInspect(ctx, id64)
	for k, v := range st.Labels {
		if strings.ContainsAny(k+v, "\x1b\x00‮") {
			t.Errorf("label %q=%q", k, v)
		}
	}
	if err != nil || strings.ContainsAny(st.StartedAt, "\x1b") {
		t.Fatalf("%+v %v", st, err)
	}
	b, _ = json.Marshal(map[string]any{"ok": false, "ms": 1, "err": dirty})
	hr, err := NewClient(&stub{result: b}).HealthCheck(ctx, HealthArgs{})
	if err != nil || strings.ContainsAny(hr.Err, "\x1b\x00‮") {
		t.Fatalf("%+v %v", hr, err)
	}
	b, _ = json.Marshal(map[string]any{"containers": []map[string]any{{"name": dirty, "ports": []map[string]any{{"host_ip": dirty, "host_port": 1, "container_port": 2, "proto": dirty}}}}})
	pr, err := NewClient(&stub{result: b}).ContainerPorts(ctx)
	if err != nil || strings.ContainsAny(pr.Containers[0].Name+pr.Containers[0].Ports[0].HostIP+pr.Containers[0].Ports[0].Proto, "\x1b\x00‮") {
		t.Fatalf("%+v %v", pr, err)
	}
}

func TestClientEventsSkipsBadLinesAndCleansTheRest(t *testing.T) {
	s := &stub{result: json.RawMessage(`{}`), lines: []agent.Line{
		{T: agent.LineEvent, E: json.RawMessage(`{"action":"die\u001b","id":"` + id64 + `","service":"7","deploy":"3","exit_code":"1","time":5}`)},
		{T: agent.LineEvent, E: json.RawMessage(`{"action":"die","id":"short","service":"","deploy":"","exit_code":"","time":5}`)},
		{T: agent.LineEvent, E: json.RawMessage(`{"action":"die","id":"` + id64 + `","extra":1}`)},
		{T: agent.LineProgress, P: json.RawMessage(`{"x":1}`)},
	}}
	var got []Event
	if err := NewClient(s).Events(context.Background(), func(e Event) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Action != "die" || got[0].Time != 5 {
		t.Fatalf("%+v", got)
	}
}

func TestClientRefusesAShortIDBeforeSending(t *testing.T) {
	s := &stub{result: json.RawMessage(`{}`)}
	c := NewClient(s)
	ctx := context.Background()
	for _, id := range []string{"", "web", id64[:63], strings.ToUpper(id64)} {
		if c.ContainerStart(ctx, id) == nil || c.ContainerStop(ctx, id, 1) == nil || c.ContainerRestart(ctx, id, 1) == nil ||
			c.ContainerRemove(ctx, id) == nil {
			t.Errorf("%q accepted", id)
		}
		if _, err := c.ContainerInspect(ctx, id); err == nil {
			t.Errorf("inspect %q accepted", id)
		}
	}
	if s.calls != 0 {
		t.Errorf("%d calls for bad ids", s.calls)
	}
}

func TestClientPassesAgentErrorsThrough(t *testing.T) {
	oe := &agent.OpError{Code: agent.CodeRefusedUnowned, Msg: "no"}
	err := NewClient(&stub{err: oe}).ContainerRemove(context.Background(), id64)
	if !IsCode(err, agent.CodeRefusedUnowned) || IsCode(err, agent.CodeConflict) || IsCode(nil, agent.CodeConflict) {
		t.Fatalf("%v", err)
	}
}
