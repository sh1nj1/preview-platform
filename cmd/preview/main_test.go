package main

import (
	"net"
	"strconv"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"feature/auth":         "feature-auth",
		"FEATURE_AUTH":         "feature-auth",
		"feat//bar__baz":       "feat-bar-baz",
		"  -trim-me-  ":        "trim-me",
		"normal-slug":          "normal-slug",
		"weird!chars#like$":    "weirdcharslike",
		"multi---dashes":       "multi-dashes",
		"":                     "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFreePortPicksUnboundPort confirms freePort returns a port we can
// actually bind on 0.0.0.0 — the surface the dev server will use.
func TestFreePortPicksUnboundPort(t *testing.T) {
	t.Setenv("PREVIEW_PORT_START", "0")
	t.Setenv("PREVIEW_PORT_END", "0") // forces fall-through to OS-allocated port
	p, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	l, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(p))
	if err != nil {
		t.Fatalf("port %d not actually free: %v", p, err)
	}
	l.Close()
}

// TestFreePortAvoidsTakenPort verifies the allocator skips a port we've
// already bound on 0.0.0.0 (which is the regression Codex flagged: the
// previous version only checked 127.0.0.1).
func TestFreePortAvoidsTakenPort(t *testing.T) {
	taken, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer taken.Close()
	takenPort := taken.Addr().(*net.TCPAddr).Port

	t.Setenv("PREVIEW_PORT_START", strconv.Itoa(takenPort))
	t.Setenv("PREVIEW_PORT_END", strconv.Itoa(takenPort))

	p, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if p == takenPort {
		t.Fatalf("freePort returned the taken port %d", p)
	}
}

// TestFirstNonLoopbackIPv4 verifies the IP detector works without the
// previous Internet probe. We don't assert a specific value (CI hosts vary)
// but it should never return a loopback or link-local address — and on any
// reasonable test host with at least one configured interface, it should
// return something non-empty.
func TestFirstNonLoopbackIPv4(t *testing.T) {
	got := firstNonLoopbackIPv4()
	if got == "" {
		t.Skip("no non-loopback IPv4 interface available in this environment")
	}
	ip := net.ParseIP(got)
	if ip == nil {
		t.Fatalf("not a valid IP: %q", got)
	}
	if ip.IsLoopback() {
		t.Fatalf("returned loopback address: %s", got)
	}
	if ip.IsLinkLocalUnicast() {
		t.Fatalf("returned link-local address: %s", got)
	}
	if ip.To4() == nil {
		t.Fatalf("not IPv4: %s", got)
	}
}

func TestDetectIPRespectsOverride(t *testing.T) {
	t.Setenv("PREVIEW_HOST_IP", "10.99.88.77")
	if got := detectIP(); got != "10.99.88.77" {
		t.Fatalf("override ignored: got %q", got)
	}
}

func TestEnvInt(t *testing.T) {
	t.Setenv("FOO_NUM", "42")
	if got := envInt("FOO_NUM", 0); got != 42 {
		t.Errorf("got %d, want 42", got)
	}
	t.Setenv("FOO_NUM", "not-a-number")
	if got := envInt("FOO_NUM", 7); got != 7 {
		t.Errorf("fallback: got %d, want 7", got)
	}
	if got := envInt("DOES_NOT_EXIST_XYZ", 99); got != 99 {
		t.Errorf("missing: got %d, want 99", got)
	}
}
