package policy

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// MaxRefLen is the longest image reference accepted (Docker's own limit).
const MaxRefLen = 255

const defaultRegistry = "docker.io"

// Ref is a parsed image reference. Host is empty when the reference names no
// registry (Docker Hub is implied); Path is the repository path as written.
// Parsing never touches the network.
type Ref struct {
	Host   string // "host", "host:port" or "[v6]:port" as written; "" = default registry
	Path   string
	Tag    string
	Digest string // "sha256:<64 hex>" or ""
}

func isAlnumLower(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }
func isAlnum(c byte) bool      { return isAlnumLower(c) || c >= 'A' && c <= 'Z' }

func shown(s string) string { return agent.CleanText(s, 48) }

// ParseRef parses an image reference with Docker's grammar: an optional
// registry host[:port], lowercase path components, a tag [\w][\w.-]{0,127}, a
// sha256 digest, at most 255 bytes in all. A reference that fails to parse is
// refused, never repaired.
func ParseRef(s string) (Ref, error) {
	var r Ref
	if s == "" || len(s) > MaxRefLen {
		return r, errors.New("an image reference must be 1 to 255 bytes")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f {
			return r, errors.New("an image reference holds only printable ASCII without spaces")
		}
	}
	name := s
	if i := strings.IndexByte(name, '@'); i >= 0 {
		r.Digest = name[i+1:]
		name = name[:i]
		if !validDigest(r.Digest) {
			return Ref{}, errors.New("the digest must be sha256:<64 lowercase hex>")
		}
	}
	// A tag is a colon after the last slash (host:port/x has its colon before it).
	if c := strings.LastIndexByte(name, ':'); c > strings.LastIndexByte(name, '/') {
		r.Tag = name[c+1:]
		name = name[:c]
		if !validTag(r.Tag) {
			return Ref{}, errors.New("the tag must match [A-Za-z0-9_][A-Za-z0-9_.-]{0,127}")
		}
	}
	if name == "" {
		return Ref{}, errors.New("the repository name is empty")
	}
	// Docker's rule for "is the first component a registry".
	if i := strings.IndexByte(name, '/'); i >= 0 {
		first := name[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" || strings.ToLower(first) != first {
			r.Host, name = first, name[i+1:]
		}
	}
	r.Path = name
	if r.Host != "" {
		if err := validHost(r.Host); err != nil {
			return Ref{}, err
		}
	}
	if err := validPath(r.Path); err != nil {
		return Ref{}, err
	}
	return r, nil
}

func validDigest(d string) bool {
	h, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		if c := h[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validTag(t string) bool {
	if t == "" || len(t) > 128 {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if isAlnum(c) || c == '_' {
			continue
		}
		if i > 0 && (c == '.' || c == '-') {
			continue
		}
		return false
	}
	return true
}

// validPath checks every component against
// [a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)* (Docker's path-component grammar).
func validPath(p string) error {
	if p == "" {
		return errors.New("the repository path is empty")
	}
	for _, comp := range strings.Split(p, "/") {
		if !validComponent(comp) {
			return errors.New("each repository path component must be lowercase letters and digits, separated by one . or _, a double __, or a run of -")
		}
	}
	return nil
}

func validComponent(c string) bool {
	n := len(c)
	for i := 0; ; {
		j := i
		for j < n && isAlnumLower(c[j]) {
			j++
		}
		if j == i {
			return false
		}
		if j == n {
			return true
		}
		switch c[j] {
		case '.':
			j++
		case '_':
			j++
			if j < n && c[j] == '_' {
				j++
			}
		case '-':
			for j < n && c[j] == '-' {
				j++
			}
		default:
			return false
		}
		i = j
	}
}

// splitHost splits "host", "host:port", "[v6]" and "[v6]:port". Port is "" when
// absent. It does not validate the host.
func splitHost(h string) (host, port string, err error) {
	if strings.HasPrefix(h, "[") {
		end := strings.IndexByte(h, ']')
		if end < 0 {
			return "", "", errors.New("unclosed [ in the registry host")
		}
		host = h[1:end]
		rest := h[end+1:]
		switch {
		case rest == "":
		case rest[0] == ':':
			port = rest[1:]
			if port == "" {
				return "", "", errors.New("empty registry port")
			}
		default:
			return "", "", errors.New("garbage after ] in the registry host")
		}
		return host, port, nil
	}
	if c := strings.LastIndexByte(h, ':'); c >= 0 {
		host, port = h[:c], h[c+1:]
		if port == "" {
			return "", "", errors.New("empty registry port")
		}
		return host, port, nil
	}
	return h, "", nil
}

func validPort(p string) bool {
	if p == "" || len(p) > 5 || p[0] == '0' {
		return false
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	return n >= 1 && n <= 65535
}

// numericLabel reports whether a host label looks like part of a numeric
// address: digits, or 0x followed by hex digits. inet_aton and friends read
// those as addresses ("169.254.43518", "0xa9fea9fe"), so a host made only of
// such labels must be a canonical dotted IPv4 address or it is refused.
func numericLabel(l string) bool {
	if l == "" {
		return false
	}
	if len(l) > 2 && (l[:2] == "0x" || l[:2] == "0X") {
		for i := 2; i < len(l); i++ {
			c := l[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(l); i++ {
		if l[i] < '0' || l[i] > '9' {
			return false
		}
	}
	return true
}

func validHost(h string) error {
	host, port, err := splitHost(h)
	if err != nil {
		return err
	}
	if port != "" && !validPort(port) {
		return errors.New("the registry port must be 1 to 65535")
	}
	if strings.HasPrefix(h, "[") {
		a, err := netip.ParseAddr(host)
		if err != nil || !a.Is6() || a.Zone() != "" {
			return errors.New("a bracketed registry host must be an IPv6 address")
		}
		return nil
	}
	if host == "" || len(host) > 253 {
		return errors.New("the registry host must be 1 to 253 bytes")
	}
	labels := strings.Split(host, ".")
	allNumeric := true
	for _, l := range labels {
		if l == "" || len(l) > 63 || !isAlnum(l[0]) || !isAlnum(l[len(l)-1]) {
			return errors.New("the registry host is not a valid name")
		}
		for i := 0; i < len(l); i++ {
			if c := l[i]; !isAlnum(c) && c != '-' {
				return errors.New("the registry host is not a valid name")
			}
		}
		if !numericLabel(l) {
			allNumeric = false
		}
	}
	if allNumeric {
		a, err := netip.ParseAddr(host)
		if err != nil || !a.Is4() {
			return errors.New("a numeric registry host must be a canonical dotted IPv4 address")
		}
	}
	return nil
}

// normHost returns the lowercase host without its default port; Docker Hub's
// names collapse to docker.io. It assumes ParseRef accepted h.
func normHost(h string) string {
	if h == "" {
		return defaultRegistry
	}
	host, port, _ := splitHost(h)
	if a, err := netip.ParseAddr(host); err == nil {
		host = a.Unmap().String() // canonical text; the zone is impossible after validHost
		if a.Is6() && !a.Is4In6() {
			host = "[" + host + "]"
		}
	} else {
		host = strings.ToLower(host)
	}
	switch host {
	case "index.docker.io", "registry-1.docker.io":
		host = defaultRegistry
	}
	if port == "443" {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return host
}

// Repository is the normalised repository, "host/path" with the default
// registry and the library/ prefix filled in, the host compared without case
// and ":443" dropped. Only the tag and digest are left out.
func (r Ref) Repository() string {
	host := normHost(r.Host)
	path := r.Path
	if host == defaultRegistry && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	return host + "/" + path
}

// String is the reference as written (without any normalisation).
func (r Ref) String() string {
	var b strings.Builder
	if r.Host != "" {
		b.WriteString(r.Host + "/")
	}
	b.WriteString(r.Path)
	if r.Tag != "" {
		b.WriteString(":" + r.Tag)
	}
	if r.Digest != "" {
		b.WriteString("@" + r.Digest)
	}
	return b.String()
}

// SameRepository reports whether a and b name the same repository after
// normalisation (nginx = docker.io/library/nginx = DOCKER.IO/library/nginx;
// registry.example:443/a/b = registry.example/a/b). Only the tag or digest may
// differ. A reference that does not parse is never the same as anything,
// itself included.
func SameRepository(a, b string) bool {
	ra, err := ParseRef(a)
	if err != nil {
		return false
	}
	rb, err := ParseRef(b)
	if err != nil {
		return false
	}
	return ra.Repository() == rb.Repository()
}

// PullRef returns the string to hand to a pull: the reference as written, with
// ":latest" added when it names neither a tag nor a digest, so the tag a deploy
// records is the one Docker pulls.
func PullRef(s string) (string, error) {
	r, err := ParseRef(s)
	if err != nil {
		return "", err
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r.String(), nil
}

// ValidateImageRef parses s and applies the registry-host rules. It returns
// the reference to pull (PullRef) on success.
func ValidateImageRef(s string, o Options) (string, error) {
	r, err := ParseRef(s)
	if err != nil {
		return "", err
	}
	if err := CheckRegistry(r.Host, o); err != nil {
		return "", err
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r.String(), nil
}

// metadataAddrs are cloud metadata and provisioning endpoints that are not
// link-local addresses. Built on first use: nothing runs at package load.
var metadataAddrs = sync.OnceValue(func() []netip.Addr {
	return []netip.Addr{
		netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud
		netip.MustParseAddr("192.0.0.192"),     // Oracle Cloud
		netip.MustParseAddr("168.63.129.16"),   // Azure wire server
		netip.MustParseAddr("fd00:ec2::254"),   // AWS IPv6
	}
})

var nat64 = sync.OnceValue(func() netip.Prefix { return netip.MustParsePrefix("64:ff9b::/96") })
var nat64Local = sync.OnceValue(func() netip.Prefix { return netip.MustParsePrefix("64:ff9b:1::/48") })

// loopbackNames are host names that resolve to this machine by convention
// (glibc's and systemd's /etc/hosts entries).
var loopbackNames = map[string]bool{
	"localhost": true, "localhost4": true, "localhost6": true,
	"localhost.localdomain": true, "localhost4.localdomain4": true, "localhost6.localdomain6": true,
	"ip6-localhost": true, "ip6-loopback": true, "ip6-localnet": true,
}

// metadataNames are cloud metadata host names, refused always.
var metadataNames = map[string]bool{"metadata": true, "metadata.google.internal": true}

// localHostname is this machine's name (os.Hostname), read once on first use.
// A variable so tests can fake it.
var localHostname = sync.OnceValue(func() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
})

func isThisHost(h string) bool {
	me := localHostname()
	if me == "" {
		return false
	}
	short, _, _ := strings.Cut(me, ".")
	return h == me || h == short
}

// embeddedV4 returns the IPv4 address an IPv6 address carries when it is one of
// the translation or compatibility forms, so it is judged as that address:
// IPv4-mapped (::ffff:a.b.c.d, unwrapped by Unmap), IPv4-compatible (::a.b.c.d),
// NAT64 64:ff9b::/96 and the local-use NAT64 prefix 64:ff9b:1::/48 (RFC 8215,
// with the address laid out as RFC 6052 does for a /48).
func embeddedV4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return a, false
	}
	b := a.As16()
	switch {
	case nat64().Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case nat64Local().Contains(a):
		return netip.AddrFrom4([4]byte{b[6], b[7], b[9], b[10]}), true
	}
	zero := true
	for _, x := range b[:12] {
		if x != 0 {
			zero = false
		}
	}
	if zero && !a.IsUnspecified() && a != netip.IPv6Loopback() {
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	return a, false
}

// CheckRegistry applies the registry-host rules to the host part of a
// reference (as written, "" = Docker Hub). Link-local and metadata IP literals
// and names (metadata.google.internal) are always refused. Loopback hosts
// (localhost and the usual aliases, this machine's own host name, 127.0.0.0/8,
// ::1, the unspecified address) are refused unless o.AllowLoopbackRegistry.
// Other host names are not resolved here: a name that points at an internal
// address is the DNS-rebinding case plan 0004 accepts; only literals and the
// well-known names can be judged without a network call.
func CheckRegistry(host string, o Options) error {
	if host == "" {
		return nil
	}
	if err := validHost(host); err != nil {
		return err
	}
	h, _, _ := splitHost(host)
	h = strings.ToLower(h)
	if metadataNames[h] {
		return errors.New("a cloud metadata host name is refused as a registry")
	}
	if loopbackNames[h] || strings.HasSuffix(h, ".localhost") || isThisHost(h) {
		if !o.AllowLoopbackRegistry {
			return errors.New("registries on this machine (localhost and its aliases) need --allow-loopback-registry")
		}
		return nil
	}
	a, err := netip.ParseAddr(h)
	if err != nil {
		return nil // a name
	}
	a = a.Unmap()
	if v4, ok := embeddedV4(a); ok {
		a = v4 // judge what it embeds
	}
	switch {
	case a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast():
		return fmt.Errorf("registry address %s is link-local, which is refused", shown(a.String()))
	case a.IsMulticast():
		return fmt.Errorf("registry address %s is a multicast address", shown(a.String()))
	}
	for _, m := range metadataAddrs() {
		if a == m {
			return fmt.Errorf("registry address %s is a cloud metadata address, which is refused", shown(a.String()))
		}
	}
	if a.IsLoopback() || a.IsUnspecified() {
		if !o.AllowLoopbackRegistry {
			return errors.New("registries on loopback addresses need --allow-loopback-registry")
		}
	}
	return nil
}
