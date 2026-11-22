package policy

import (
	"net/netip"
	"strings"
	"testing"
)

const dig = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSameRepositoryTable(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"nginx", "docker.io/library/nginx", true},
		{"docker.io/library/nginx", "DOCKER.IO/library/nginx", true},
		{"nginx", "DOCKER.IO/library/nginx", true},
		{"nginx", "index.docker.io/library/nginx", true},
		{"nginx", "registry-1.docker.io/library/nginx:1", true},
		{"nginx:1", "nginx:2", true},
		{"nginx:1", "nginx@" + dig, true},
		{"nginx@" + dig, "nginx:latest@" + dig, true},
		{"registry.example:443/a/b", "registry.example/a/b", true},
		{"registry.example:443/a/b", "REGISTRY.example/a/b:1", true},
		{"registry.example:5000/a/b", "registry.example/a/b", false},
		{"ghcr.io/a/b", "ghcr.io/a/b:v1", true},
		{"ghcr.io/a/b", "ghcr.io/a/c", false},
		{"ghcr.io/a/b", "quay.io/a/b", false},
		{"nginx", "example/nginx", false},
		{"nginx", "library/nginx", true},
		{"user/app", "docker.io/user/app", true},
		{"user/app", "docker.io/User/app", false}, // an uppercase path does not parse
		{"[::1]:5000/a", "[0:0:0:0:0:0:0:1]:5000/a", true},
		{"127.0.0.1:5000/a", "127.0.0.1:5000/a:1", true},
		{"localhost:5000/a", "localhost:5000/b", false},
		{"a/b", "a/b/c", false},
		// unparsable: never the same, not even as itself
		{"", "", false},
		{"NGINX", "NGINX", false},
		{"nginx:", "nginx", false},
		{"nginx@sha256:abc", "nginx", false},
		{"nginx@sha256:abc", "nginx@sha256:abc", false},
		{"a b", "a b", false},
		{"nginx", "nginx\x00", false},
		{"host:99999/a", "host:99999/a", false},
		{"-a", "-a", false},
	} {
		if got := SameRepository(c.a, c.b); got != c.same {
			t.Errorf("SameRepository(%q, %q) = %v, want %v", c.a, c.b, got, c.same)
		}
		if got := SameRepository(c.b, c.a); got != c.same {
			t.Errorf("SameRepository(%q, %q) = %v, want %v (symmetry)", c.b, c.a, got, c.same)
		}
	}
}

func TestParseRefAcceptsAndRefuses(t *testing.T) {
	for _, s := range []string{
		"nginx", "nginx:1.27", "a/b", "a.b/c", "localhost/a", "localhost:5000/a/b:tag", "127.0.0.1:5000/x",
		"ghcr.io/org/app_v2.x--y:tag-1_2.3", "a__b", "[::1]:5000/a", "registry.example:443/a",
		"nginx@" + dig, "ghcr.io/o/a:1@" + dig, "MyReg.Example/a", "a/b/c/d/e",
		strings.Repeat("a", 255), "x:" + strings.Repeat("a", 128),
	} {
		if _, err := ParseRef(s); err != nil {
			t.Errorf("%q refused: %v", s, err)
		}
	}
	for _, s := range []string{
		"", "NGINX", "Nginx:1", "a b", "a\tb", "a\nb", "é", "nginx:", ":1", "@" + dig, "nginx@", "nginx@sha256:", "nginx@sha512:" + strings.Repeat("a", 128),
		"nginx@sha256:" + strings.Repeat("A", 64), "nginx:-x", "nginx:.x", "x:" + strings.Repeat("a", 129), strings.Repeat("a", 256),
		"a//b", "/a", "a/", "a..b", "a._b", "a_.b", "a___b", "-a", "a-", ".a", "a.", "_a", "a/-b",
		"host:0/a", "host:00080/a", "host:65536/a", "host:/a", "host:x/a", "ho st/a", "-h.com/a", "h-.com/a", "h..com/a",
		"[::1/a", "[::1]x/a", "[zzz]:5000/a", "[1.2.3.4]:5000/a", "[fe80::1%25eth0]:5000/a",
		"2852039166:5000/a", "169.254.43518/a", "1.2.3/a", "010.0.0.1/a", "169.254.169.254.1/a",
		"a@b@c", "a:b:c", "http://a/b", "a/b?x", "a/b#x", "a/b\\c", "a/b:c/d",
	} {
		if _, err := ParseRef(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestPullRefAddsLatestOnlyToBareNames(t *testing.T) {
	for in, want := range map[string]string{
		"nginx":                  "nginx:latest",
		"user/app":               "user/app:latest",
		"ghcr.io/o/a":            "ghcr.io/o/a:latest",
		"localhost:5000/a":       "localhost:5000/a:latest",
		"[::1]:5000/a":           "[::1]:5000/a:latest",
		"nginx:1":                "nginx:1",
		"nginx@" + dig:           "nginx@" + dig,
		"nginx:1@" + dig:         "nginx:1@" + dig,
		"Registry.Example:443/a": "Registry.Example:443/a:latest",
	} {
		got, err := PullRef(in)
		if err != nil || got != want {
			t.Errorf("PullRef(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := PullRef("NGINX"); err == nil {
		t.Error("an unparsable reference was normalised")
	}
}

func TestRegistryHostRules(t *testing.T) {
	deny := Options{}
	allow := Options{AllowLoopbackRegistry: true}
	always := []string{ // refused even with the flag
		"169.254.169.254", "169.254.169.254:5000", "169.254.0.1", "169.254.255.255", "[fe80::1]:5000", "[fe80::abcd]",
		"[fd00:ec2::254]", "100.100.100.200", "192.0.0.192", "168.63.129.16", "[::ffff:169.254.169.254]",
		"[::ffff:a9fe:a9fe]", "[64:ff9b::a9fe:a9fe]", "224.0.0.1", "[ff02::1]", "[ff01::1]",
	}
	for _, h := range always {
		for _, o := range []Options{deny, allow} {
			if err := CheckRegistry(h, o); err == nil {
				t.Errorf("%s accepted (allow=%v)", h, o.AllowLoopbackRegistry)
			}
		}
	}
	loop := []string{"localhost", "localhost:5000", "LOCALHOST:5000", "127.0.0.1", "127.0.0.1:5000", "127.1.2.3", "[::1]", "[::1]:5000",
		"[::ffff:127.0.0.1]", "foo.localhost", "0.0.0.0", "[::]", "[64:ff9b::7f00:1]"}
	for _, h := range loop {
		if err := CheckRegistry(h, deny); err == nil {
			t.Errorf("%s accepted without the flag", h)
		}
		if err := CheckRegistry(h, allow); err != nil {
			t.Errorf("%s refused with the flag: %v", h, err)
		}
	}
	for _, h := range []string{"", "docker.io", "ghcr.io", "registry.example:5000", "10.0.0.5:5000", "192.168.1.10", "[2001:db8::1]:5000", "8.8.8.8"} {
		if err := CheckRegistry(h, deny); err != nil {
			t.Errorf("%s refused: %v", h, err)
		}
	}
	// ValidateImageRef applies both the grammar and the host rules, and
	// returns the reference to pull.
	if got, err := ValidateImageRef("127.0.0.1:5000/sd-testapp", allow); err != nil || got != "127.0.0.1:5000/sd-testapp:latest" {
		t.Errorf("%q %v", got, err)
	}
	for _, s := range []string{"127.0.0.1:5000/x", "localhost/x", "169.254.169.254/x", "[::1]:5000/x"} {
		if _, err := ValidateImageRef(s, deny); err == nil {
			t.Errorf("%s accepted without the flag", s)
		}
	}
	if _, err := ValidateImageRef("169.254.169.254/x", allow); err == nil {
		t.Error("a metadata registry accepted with the loopback flag")
	}
}

func FuzzImageRef(f *testing.F) {
	for _, s := range []string{"nginx", "a/b:1", "localhost:5000/a", "[::1]:5000/a@" + dig, "169.254.169.254/x", "Reg.EX:443/a/b", "", "a b", "0xa9fea9fe/a"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := ParseRef(s)
		pull, perr := PullRef(s)
		if (err == nil) != (perr == nil) {
			t.Fatalf("ParseRef and PullRef disagree on %q", s)
		}
		_, verr := ValidateImageRef(s, Options{})
		if err != nil {
			if verr == nil {
				t.Fatalf("ValidateImageRef accepted %q that ParseRef refuses", s)
			}
			return
		}
		if len(s) > MaxRefLen || strings.ContainsAny(s, " \t\r\n\x00") {
			t.Fatalf("accepted %q", s)
		}
		if r.String() != s {
			t.Fatalf("round trip %q -> %q", s, r.String())
		}
		// The pull form parses, names the same repository, and always has a tag or digest.
		pr, err := ParseRef(pull)
		if err != nil || pr.Repository() != r.Repository() || (pr.Tag == "" && pr.Digest == "") {
			t.Fatalf("pull form %q of %q", pull, s)
		}
		if !SameRepository(s, pull) || !SameRepository(s, s) {
			t.Fatalf("not the same repository as itself or its pull form: %q", s)
		}
		// Without the flag, no loopback, link-local or metadata literal gets through.
		if verr == nil {
			if err := CheckRegistry(r.Host, Options{AllowLoopbackRegistry: true}); err != nil {
				t.Fatalf("accepted a host the flag cannot accept: %q", s)
			}
			h, _, _ := splitHost(r.Host)
			if a, err := netip.ParseAddr(h); err == nil {
				a = a.Unmap()
				if a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() {
					t.Fatalf("accepted %q without the flag", s)
				}
			} else if h == "localhost" || strings.HasSuffix(h, ".localhost") {
				t.Fatalf("accepted %q without the flag", s)
			}
			// A host made of numeric labels is an address or it is refused:
			// resolvers read "169.254.43518" as one.
			if labels := strings.Split(h, "."); func() bool {
				for _, l := range labels {
					if !numericLabel(l) {
						return false
					}
				}
				return true
			}() {
				if a, err := netip.ParseAddr(h); err != nil || !a.Is4() {
					t.Fatalf("accepted the numeric host in %q", s)
				}
			}
		}
	})
}

func FuzzSameRepository(f *testing.F) {
	for _, p := range [][2]string{{"nginx", "docker.io/library/nginx"}, {"a:1", "a:2"}, {"h:443/a", "h/a"}, {"", ""}, {"A", "a"}, {"[::1]:1/a", "[::1]:1/a"}} {
		f.Add(p[0], p[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		ab, ba := SameRepository(a, b), SameRepository(b, a)
		if ab != ba {
			t.Fatalf("not symmetric: %q %q", a, b)
		}
		ra, ea := ParseRef(a)
		rb, eb := ParseRef(b)
		if (ea != nil || eb != nil) && ab {
			t.Fatalf("an unparsable reference is the same as something: %q %q", a, b)
		}
		if ea == nil && eb == nil && ab != (ra.Repository() == rb.Repository()) {
			t.Fatalf("disagrees with the normalised repository: %q %q", a, b)
		}
		if ea == nil && !SameRepository(a, a) {
			t.Fatalf("not reflexive: %q", a)
		}
		// Only the tag or digest may differ.
		if ea == nil && ra.Tag == "" && ra.Digest == "" && len(a) < 100 {
			if !SameRepository(a, a+":v1") || !SameRepository(a, a+"@"+dig) {
				t.Fatalf("a tag or digest changed the repository of %q", a)
			}
		}
		// Transitivity through the normal form.
		if ab && ea == nil && ra.Repository() != rb.Repository() {
			t.Fatalf("same but different repositories: %q %q", a, b)
		}
	})
}

func TestLoopbackNamesAndThisHost(t *testing.T) {
	old := localHostname
	localHostname = func() string { return "build-box.example.net" }
	t.Cleanup(func() { localHostname = old })
	names := []string{"localhost4", "localhost6", "localhost.localdomain", "localhost4.localdomain4", "localhost6.localdomain6",
		"ip6-localhost", "ip6-loopback", "ip6-localnet", "IP6-LOCALHOST:5000", "build-box", "BUILD-BOX:5000", "build-box.example.net", "Build-Box.Example.Net"}
	for _, h := range names {
		if err := CheckRegistry(h, Options{}); err == nil {
			t.Errorf("%s accepted without the flag", h)
		}
		if err := CheckRegistry(h, Options{AllowLoopbackRegistry: true}); err != nil {
			t.Errorf("%s refused with the flag: %v", h, err)
		}
	}
	for _, h := range []string{"metadata.google.internal", "METADATA.google.internal:80", "metadata"} {
		for _, o := range []Options{{}, {AllowLoopbackRegistry: true}} {
			if err := CheckRegistry(h, o); err == nil {
				t.Errorf("%s accepted (%+v)", h, o)
			}
		}
	}
	for _, h := range []string{"other-box", "build-box2", "example.net", "registry.build-box.example.net"} {
		if err := CheckRegistry(h, Options{}); err != nil {
			t.Errorf("%s refused: %v", h, err)
		}
	}
}

func TestTranslationFormsAreJudgedByTheAddressTheyCarry(t *testing.T) {
	always := []string{
		"[::169.254.169.254]", "[::a9fe:a9fe]", "[::100.100.100.200]", "[::168.63.129.16]",
		"[64:ff9b::a9fe:a9fe]", "[64:ff9b:1:a9fe:a9:fe00:0:0]", // RFC 8215 /48 layout: 169.254 | u | 169.254
	}
	for _, h := range always {
		if err := CheckRegistry(h, Options{AllowLoopbackRegistry: true}); err == nil {
			t.Errorf("%s accepted with the flag", h)
		}
	}
	loop := []string{"[::127.0.0.1]", "[::7f00:1]", "[64:ff9b:1:7f00:0:100::]", "[64:ff9b:1:7f00:1::]"}
	for _, h := range loop {
		if err := CheckRegistry(h, Options{}); err == nil {
			t.Errorf("%s accepted without the flag", h)
		}
	}
	// ::1 and :: keep their own rules; an ordinary compatible address passes.
	if err := CheckRegistry("[::8.8.8.8]", Options{}); err != nil {
		t.Errorf("[::8.8.8.8]: %v", err)
	}
	if err := CheckRegistry("[::1]", Options{AllowLoopbackRegistry: true}); err != nil {
		t.Errorf("[::1]: %v", err)
	}
}
