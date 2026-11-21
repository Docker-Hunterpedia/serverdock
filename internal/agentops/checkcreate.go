package agentops

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// CreateExpect is what the agent knows when it checks a create body: the pieces
// it derived itself (instance, network) and the ones the caller named (address,
// image id). The agent has no deploy row, so label values other than the
// instance are checked for shape only; the control plane checks them against
// its expectations before it sends the request.
type CreateExpect struct {
	Instance string // this install's id (the agent sets the label itself)
	Network  string // the app network's name
	IP       string // the static address (already checked against the subnet)
	ImageID  string // the expected sha256:<64 hex> image id
}

// CheckCreateFunc is the policy hook: it gets the exact bytes about to be sent
// to Docker and returns nil only if every part of the body is allowed. Step 5
// plugs policy.CheckCreate in here. There is deliberately no "allow all"
// implementation: Deps.CheckCreate is required.
type CheckCreateFunc func(body []byte, exp CreateExpect) error

var (
	labelValueRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	portKeyRe    = regexp.MustCompile(`^([0-9]{1,5})/tcp$`)
)

// InterimCheckCreate is the strict default-deny check the agent runs until
// policy.CheckCreate lands (plan 0004 step 5, which replaces it): the body,
// decoded into a generic tree, must equal the one allowed shape, key for key
// and value for value. Anything extra (a key anywhere, a seventh label, another
// network, another address) or different is refused, and so is a body that
// repeats a key. It is not a stub: it refuses everything the plan's allowlist
// refuses.
func InterimCheckCreate(body []byte, exp CreateExpect) error {
	if exp.Instance == "" || exp.Network == "" || exp.IP == "" || exp.ImageID == "" {
		return errors.New("incomplete expectations")
	}
	if err := noDuplicateKeys(body); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		return fmt.Errorf("body is not a JSON object: %w", err)
	}
	if dec.More() {
		return errors.New("trailing data after the body")
	}
	// Labels: exactly six keys; values have the expected shape.
	labels, _ := got["Labels"].(map[string]any)
	if len(labels) != 6 {
		return fmt.Errorf("labels: want exactly 6, have %d", len(labels))
	}
	wantLabels := map[string]any{}
	for _, k := range append([]string{LabelInstance}, callerLabels...) {
		v, ok := labels[k].(string)
		if !ok {
			return fmt.Errorf("labels: %s is missing", k)
		}
		switch {
		case k == LabelManaged && v != "1":
			return errors.New("labels: serverdock.managed must be 1")
		case k == LabelInstance && v != exp.Instance:
			return errors.New("labels: serverdock.instance is not this install's")
		case k != LabelManaged && k != LabelInstance && !labelValueRe.MatchString(v):
			return fmt.Errorf("labels: %s has a bad value", k)
		}
		wantLabels[k] = v
	}
	want := map[string]any{
		"Image":  exp.ImageID,
		"Labels": wantLabels,
		"HostConfig": map[string]any{
			"RestartPolicy": map[string]any{"Name": "unless-stopped"},
			"LogConfig": map[string]any{"Type": "local", "Config": map[string]any{
				"max-size": "10m", "max-file": "3"}},
			"NetworkMode": exp.Network,
			"PidsLimit":   json.Number("4096"),
		},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{
			exp.Network: map[string]any{"IPAMConfig": map[string]any{"IPv4Address": exp.IP}},
		}},
	}
	if ep, ok := got["ExposedPorts"]; ok {
		m, _ := ep.(map[string]any)
		if len(m) != 1 {
			return errors.New("ExposedPorts: want exactly one port")
		}
		for k, v := range m {
			sub := portKeyRe.FindStringSubmatch(k)
			if sub == nil {
				return errors.New("ExposedPorts: bad port key")
			}
			if n, _ := strconv.Atoi(sub[1]); n < 1 || n > 65535 {
				return errors.New("ExposedPorts: port out of range")
			}
			if mv, ok := v.(map[string]any); !ok || len(mv) != 0 {
				return errors.New("ExposedPorts: value must be {}")
			}
		}
		want["ExposedPorts"] = ep
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("the create body differs from the allowed shape (%s)", firstDifference(got, want))
	}
	return nil
}

// firstDifference names the first top-level or HostConfig key that differs,
// without echoing any value.
func firstDifference(got, want map[string]any) string {
	for k := range got {
		if _, ok := want[k]; !ok {
			return "unexpected key " + quoteKey(k)
		}
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			return "missing key " + k
		}
		if reflect.DeepEqual(g, w) {
			continue
		}
		gm, gok := g.(map[string]any)
		wm, wok := w.(map[string]any)
		if gok && wok {
			return k + ": " + firstDifference(gm, wm)
		}
		return "wrong value for " + k
	}
	return "unknown"
}

func quoteKey(k string) string {
	k = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, k)
	if len(k) > 48 {
		k = k[:48]
	}
	return strconv.Quote(k)
}

// noDuplicateKeys refuses a repeated key at any depth (encoding/json keeps the
// last, so a check on the decoded tree could differ from what Docker reads).
func noDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		if d == '[' {
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err := dec.Token()
			return err
		}
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k, _ := kt.(string)
			if seen[k] {
				return errors.New("duplicate key in the body")
			}
			seen[k] = true
			if err := walk(); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	return walk()
}
