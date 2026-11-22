package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Label keys on every container ServerDock creates.
const (
	LabelManaged  = "serverdock.managed"
	LabelInstance = "serverdock.instance"
	LabelProject  = "serverdock.project"
	LabelService  = "serverdock.service"
	LabelDeploy   = "serverdock.deploy"
	LabelAttempt  = "serverdock.attempt"
)

// CreateExpect is what the checker knows about the create it is judging.
//
// The control plane fills in everything (it has the deploy row); the agent
// fills in what it derived itself (Instance, Network, the subnet facts) plus
// what the request named (IP, ImageID), and leaves Project, Service, Deploy,
// Attempt and ContainerPort empty, in which case those label values are checked
// for shape only.
type CreateExpect struct {
	Instance string // the install id: the agent sets serverdock.instance to it
	Network  string // the app network's name
	IP       string // the static IPv4 address, canonical text
	ImageID  string // the expected sha256:<64 hex> image id

	Subnet  string // meta.app_subnet (required)
	Gateway string // optional; the network's gateway as Docker reports it
	IPRange string // optional; the network's dynamic range as Docker reports it

	Project, Service, Deploy, Attempt string // label values (decimal ids)
	ContainerPort                     int    // 0 = any single TCP port (or none)
}

// Limits on the body the checker is willing to parse. The one allowed shape is
// under 2 KiB; a hostile body is cut off long before it costs anything.
const (
	maxCreateBody  = 64 << 10
	maxKeysPerObj  = 256
	maxArrayItems  = 256
	maxCreateDepth = 16
)

var (
	networkNameRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`) })
	imageIDRe     = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^sha256:[0-9a-f]{64}$`) })
	instanceRe    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9a-f]{32}$`) })
	idLabelRe     = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[1-9][0-9]{0,17}$`) })
)

// CheckCreateControlPlane is CheckCreate as the control plane runs it at the
// deploy's check stage: exactly five labels (managed, project, service, deploy,
// attempt) with the expected values. The agent adds serverdock.instance later.
func CheckCreateControlPlane(body []byte, exp CreateExpect) error {
	return checkCreate(body, exp, false)
}

// CheckCreateAgent is CheckCreate as the agent runs it on the exact bytes it
// sends to Docker: exactly six labels, the sixth being serverdock.instance with
// this install's id.
func CheckCreateAgent(body []byte, exp CreateExpect) error {
	return checkCreate(body, exp, true)
}

// checkCreate is default-deny on the final Docker create body, down to the
// leaves. The body is parsed into a generic tree (a duplicate key, or two keys
// that differ only in case, anywhere, is refused, because encoding/json and
// Docker would each pick one) and every path must match the allowlist exactly.
// A key that is not on the list, however spelled, refuses the body. Errors name
// a path or a key (cleaned and cut) and never echo a value.
func checkCreate(body []byte, exp CreateExpect, agent bool) error {
	if err := exp.validate(agent); err != nil {
		return err
	}
	root, err := parseStrict(body)
	if err != nil {
		return err
	}
	if err := keysOf(root, "body", []string{"Image", "Labels", "HostConfig", "NetworkingConfig"}, []string{"ExposedPorts"}); err != nil {
		return err
	}
	if s, ok := root["Image"].(string); !ok || s != exp.ImageID {
		return errors.New("Image must be the expected image id")
	}
	if err := checkLabels(root["Labels"], exp, agent); err != nil {
		return err
	}
	if ep, ok := root["ExposedPorts"]; ok {
		if err := checkExposed(ep, exp.ContainerPort); err != nil {
			return err
		}
	}
	if err := checkHostConfig(root["HostConfig"], exp); err != nil {
		return err
	}
	return checkNetworking(root["NetworkingConfig"], exp)
}

func (e CreateExpect) validate(agent bool) error {
	if !networkNameRe().MatchString(e.Network) {
		return errors.New("incomplete expectations: network")
	}
	if !imageIDRe().MatchString(e.ImageID) {
		return errors.New("incomplete expectations: image id")
	}
	ip, err := netip.ParseAddr(e.IP)
	if err != nil || !ip.Is4() || ip.String() != e.IP {
		return errors.New("incomplete expectations: address")
	}
	if agent && !instanceRe().MatchString(e.Instance) {
		return errors.New("incomplete expectations: instance")
	}
	if err := StaticAddr(e.Subnet, e.Gateway, e.IPRange, ip); err != nil {
		return fmt.Errorf("address refused: %w", err)
	}
	for _, v := range []struct{ name, val string }{
		{"project", e.Project}, {"service", e.Service}, {"deploy", e.Deploy}, {"attempt", e.Attempt},
	} {
		switch {
		case v.val == "" && !agent:
			return fmt.Errorf("incomplete expectations: %s label", v.name)
		case v.val != "" && !idLabelRe().MatchString(v.val):
			return fmt.Errorf("incomplete expectations: %s label", v.name)
		}
	}
	if e.ContainerPort != 0 && ValidateContainerPort(e.ContainerPort) != nil {
		return errors.New("incomplete expectations: container port")
	}
	return nil
}

func checkLabels(v any, exp CreateExpect, agent bool) error {
	m, ok := v.(map[string]any)
	if !ok {
		return errors.New("Labels must be an object")
	}
	want := []string{LabelManaged, LabelProject, LabelService, LabelDeploy, LabelAttempt}
	if agent {
		want = append(want, LabelInstance)
	}
	if err := keysOf(m, "Labels", want, nil); err != nil {
		return err
	}
	expected := map[string]string{
		LabelProject: exp.Project, LabelService: exp.Service, LabelDeploy: exp.Deploy, LabelAttempt: exp.Attempt,
		LabelInstance: exp.Instance,
	}
	for _, k := range want {
		s, ok := m[k].(string)
		if !ok {
			return fmt.Errorf("label %s must be a string", k)
		}
		switch {
		case k == LabelManaged:
			if s != "1" {
				return errors.New("label serverdock.managed must be 1")
			}
		case k == LabelInstance:
			if s != exp.Instance {
				return errors.New("label serverdock.instance is not this install's")
			}
		case expected[k] != "":
			if s != expected[k] {
				return fmt.Errorf("label %s has an unexpected value", k)
			}
		case !idLabelRe().MatchString(s):
			return fmt.Errorf("label %s must be a decimal id", k)
		}
	}
	return nil
}

func checkExposed(v any, port int) error {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return errors.New("ExposedPorts must hold exactly one port")
	}
	for k, val := range m {
		n, ok := strings.CutSuffix(k, "/tcp")
		if !ok || !validPort(n) {
			return errors.New("ExposedPorts must name one TCP port")
		}
		if p, _ := strconv.Atoi(n); port != 0 && p != port {
			return errors.New("ExposedPorts is not the service's container port")
		}
		if mv, ok := val.(map[string]any); !ok || len(mv) != 0 {
			return errors.New("an ExposedPorts value must be {}")
		}
	}
	return nil
}

func checkHostConfig(v any, exp CreateExpect) error {
	hc, ok := v.(map[string]any)
	if !ok {
		return errors.New("HostConfig must be an object")
	}
	if err := keysOf(hc, "HostConfig", []string{"RestartPolicy", "LogConfig", "NetworkMode", "PidsLimit"}, nil); err != nil {
		return err
	}
	rp, ok := hc["RestartPolicy"].(map[string]any)
	if !ok {
		return errors.New("HostConfig.RestartPolicy must be an object")
	}
	if err := keysOf(rp, "HostConfig.RestartPolicy", []string{"Name"}, []string{"MaximumRetryCount"}); err != nil {
		return err
	}
	if rp["Name"] != "unless-stopped" {
		return errors.New("HostConfig.RestartPolicy.Name must be unless-stopped")
	}
	if n, ok := rp["MaximumRetryCount"]; ok && n != json.Number("0") {
		return errors.New("HostConfig.RestartPolicy.MaximumRetryCount must be 0")
	}
	lc, ok := hc["LogConfig"].(map[string]any)
	if !ok {
		return errors.New("HostConfig.LogConfig must be an object")
	}
	if err := keysOf(lc, "HostConfig.LogConfig", []string{"Type", "Config"}, nil); err != nil {
		return err
	}
	if lc["Type"] != "local" {
		return errors.New("HostConfig.LogConfig.Type must be local")
	}
	lcc, ok := lc["Config"].(map[string]any)
	if !ok {
		return errors.New("HostConfig.LogConfig.Config must be an object")
	}
	if err := keysOf(lcc, "HostConfig.LogConfig.Config", []string{"max-size", "max-file"}, nil); err != nil {
		return err
	}
	if lcc["max-size"] != "10m" || lcc["max-file"] != "3" {
		return errors.New("HostConfig.LogConfig.Config must be max-size 10m and max-file 3")
	}
	if hc["NetworkMode"] != exp.Network {
		return errors.New("HostConfig.NetworkMode must be the app network")
	}
	if hc["PidsLimit"] != json.Number(strconv.Itoa(PidsLimit)) {
		return fmt.Errorf("HostConfig.PidsLimit must be %d", PidsLimit)
	}
	return nil
}

func checkNetworking(v any, exp CreateExpect) error {
	nc, ok := v.(map[string]any)
	if !ok {
		return errors.New("NetworkingConfig must be an object")
	}
	if err := keysOf(nc, "NetworkingConfig", []string{"EndpointsConfig"}, nil); err != nil {
		return err
	}
	ec, ok := nc["EndpointsConfig"].(map[string]any)
	if !ok {
		return errors.New("NetworkingConfig.EndpointsConfig must be an object")
	}
	if err := keysOf(ec, "NetworkingConfig.EndpointsConfig", []string{exp.Network}, nil); err != nil {
		return err
	}
	ep, ok := ec[exp.Network].(map[string]any)
	if !ok {
		return errors.New("the endpoint config must be an object")
	}
	if err := keysOf(ep, "the endpoint config", []string{"IPAMConfig"}, nil); err != nil {
		return err
	}
	ipam, ok := ep["IPAMConfig"].(map[string]any)
	if !ok {
		return errors.New("IPAMConfig must be an object")
	}
	if err := keysOf(ipam, "IPAMConfig", []string{"IPv4Address"}, nil); err != nil {
		return err
	}
	if ipam["IPv4Address"] != exp.IP {
		return errors.New("IPAMConfig.IPv4Address must be the allocated address")
	}
	return nil
}

// keysOf requires every key in must, allows those in may, and refuses any
// other key. Key matching is exact: a key that differs from an allowed one only
// in case is refused as such (Docker's decoder would read it as the allowed key).
func keysOf(m map[string]any, where string, must, may []string) error {
	allowed := make(map[string]bool, len(must)+len(may))
	for _, k := range must {
		allowed[k] = true
		if _, ok := m[k]; !ok {
			return fmt.Errorf("%s: missing %s", where, shown(k))
		}
	}
	for _, k := range may {
		allowed[k] = true
	}
	for k := range m {
		if allowed[k] {
			continue
		}
		for a := range allowed {
			if strings.EqualFold(a, k) {
				return fmt.Errorf("%s: key %q differs from %q only in case", where, shown(k), a)
			}
		}
		return fmt.Errorf("%s: %q is not allowed", where, shown(k))
	}
	return nil
}

// parseStrict decodes exactly one JSON object into a tree of map[string]any,
// []any, string, json.Number, bool and nil. It refuses a duplicate key at any
// depth (inside arrays too), two keys of one object that differ only in case,
// more than one top-level value, and anything over the caps above.
func parseStrict(b []byte) (map[string]any, error) {
	if len(b) == 0 || len(b) > maxCreateBody {
		return nil, errors.New("the create body is empty or too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := readValue(dec, 0)
	if err != nil {
		return nil, fmt.Errorf("the create body is not valid JSON: %w", simpleErr(err))
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("the create body is not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the create body")
	}
	return m, nil
}

// simpleErr keeps a parse error's text short; encoding/json errors can quote
// input bytes, which must not reach a log or a response.
func simpleErr(err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return errors.New("syntax error")
	}
	if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("unexpected end")
	}
	var pe parseError
	if errors.As(err, &pe) {
		return pe
	}
	return errors.New("invalid")
}

type parseError string

func (e parseError) Error() string { return string(e) }

func readValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxCreateDepth {
		return nil, parseError("nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	if d == '[' {
		var arr []any
		for dec.More() {
			if len(arr) == maxArrayItems {
				return nil, parseError("an array is too long")
			}
			v, err := readValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	obj := map[string]any{}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := kt.(string)
		if len(keys) == maxKeysPerObj {
			return nil, parseError("an object has too many keys")
		}
		for _, p := range keys {
			if p == k {
				return nil, parseError("duplicate key " + strconv.Quote(shown(k)))
			}
			if strings.EqualFold(p, k) {
				return nil, parseError("keys that differ only in case: " + strconv.Quote(shown(k)))
			}
		}
		keys = append(keys, k)
		v, err := readValue(dec, depth+1)
		if err != nil {
			return nil, err
		}
		obj[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return obj, nil
}
