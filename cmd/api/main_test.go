package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	return &server{cfg: config{
		Token:        "secret",
		Domain:       "preview.example.com",
		DynamicDir:   dir,
		PublicAPIURL: "http://localhost:8080",
	}}, dir
}

func do(s *server, method, path, token string, body any) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	return w
}

func TestValidSlug(t *testing.T) {
	cases := map[string]bool{
		"a":             true,
		"abc":           true,
		"abc-123":       true,
		"a1":            true,
		"":              false,
		"-abc":          false,
		"abc-":          false,
		"abc_def":       false,
		"abc.def":       false,
		"ABC":           false,
		strings.Repeat("a", 64): false,
		strings.Repeat("a", 63): true,
	}
	for in, want := range cases {
		if got := validSlug(in); got != want {
			t.Errorf("validSlug(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestAuth(t *testing.T) {
	s, _ := newTestServer(t)
	t.Run("no token", func(t *testing.T) {
		w := do(s, "GET", "/v1/previews", "", nil)
		if w.Code != 401 {
			t.Fatalf("got %d, want 401", w.Code)
		}
	})
	t.Run("wrong token", func(t *testing.T) {
		w := do(s, "GET", "/v1/previews", "nope", nil)
		if w.Code != 401 {
			t.Fatalf("got %d, want 401", w.Code)
		}
	})
	t.Run("right token", func(t *testing.T) {
		w := do(s, "GET", "/v1/previews", "secret", nil)
		if w.Code != 200 {
			t.Fatalf("got %d, want 200", w.Code)
		}
	})
	t.Run("healthz no auth", func(t *testing.T) {
		w := do(s, "GET", "/healthz", "", nil)
		if w.Code != 200 {
			t.Fatalf("got %d, want 200", w.Code)
		}
	})
}

func TestCreateGetDelete(t *testing.T) {
	s, dir := newTestServer(t)

	w := do(s, "POST", "/v1/previews", "secret", linkReq{
		Project: "myrepo", Slug: "feature-auth", Upstream: "http://10.0.0.1:3001",
	})
	if w.Code != 200 {
		t.Fatalf("create: got %d (%s)", w.Code, w.Body.String())
	}
	var lr linkResp
	json.NewDecoder(w.Body).Decode(&lr)
	if lr.URL != "https://feature-auth.myrepo.preview.example.com" {
		t.Fatalf("unexpected url: %s", lr.URL)
	}

	wantFile := filepath.Join(dir, "wt-myrepo__feature-auth.yml")
	if _, err := os.Stat(wantFile); err != nil {
		t.Fatalf("file not written: %v", err)
	}

	w = do(s, "GET", "/v1/previews/myrepo/feature-auth", "secret", nil)
	if w.Code != 200 {
		t.Fatalf("get: got %d", w.Code)
	}
	json.NewDecoder(w.Body).Decode(&lr)
	if lr.Upstream != "http://10.0.0.1:3001" {
		t.Fatalf("upstream lost: %s", lr.Upstream)
	}

	w = do(s, "DELETE", "/v1/previews/myrepo/feature-auth", "secret", nil)
	if w.Code != 204 {
		t.Fatalf("delete: got %d", w.Code)
	}
	if _, err := os.Stat(wantFile); !os.IsNotExist(err) {
		t.Fatalf("file still present after delete: %v", err)
	}

	w = do(s, "DELETE", "/v1/previews/myrepo/feature-auth", "secret", nil)
	if w.Code != 404 {
		t.Fatalf("re-delete: got %d, want 404", w.Code)
	}
}

func TestCreateValidation(t *testing.T) {
	s, _ := newTestServer(t)
	cases := []struct {
		name string
		body linkReq
	}{
		{"bad project", linkReq{Project: "Bad_Project", Slug: "ok", Upstream: "http://x:1"}},
		{"bad slug", linkReq{Project: "ok", Slug: "Bad", Upstream: "http://x:1"}},
		{"non-http upstream", linkReq{Project: "ok", Slug: "ok", Upstream: "tcp://x:1"}},
		{"upstream with newline", linkReq{Project: "ok", Slug: "ok", Upstream: "http://1.2.3.4:80\nrouters: {}"}},
		{"upstream with quote", linkReq{Project: "ok", Slug: "ok", Upstream: "http://1.2.3.4\":80"}},
		{"upstream with userinfo", linkReq{Project: "ok", Slug: "ok", Upstream: "http://user:pass@1.2.3.4:80"}},
		{"upstream with path", linkReq{Project: "ok", Slug: "ok", Upstream: "http://1.2.3.4:80/api"}},
		{"upstream with query", linkReq{Project: "ok", Slug: "ok", Upstream: "http://1.2.3.4:80?q=1"}},
		{"upstream missing host", linkReq{Project: "ok", Slug: "ok", Upstream: "http:///"}},
		{"upstream invalid port", linkReq{Project: "ok", Slug: "ok", Upstream: "http://1.2.3.4:abc"}},
		{"upstream port out of range", linkReq{Project: "ok", Slug: "ok", Upstream: "http://1.2.3.4:99999"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(s, "POST", "/v1/previews", "secret", tc.body)
			if w.Code != 400 {
				t.Fatalf("got %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestSanitizeUpstream(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"http://1.2.3.4:3001", "http://1.2.3.4:3001", false},
		{"https://example.com", "https://example.com", false},
		{"http://example.com:8080", "http://example.com:8080", false},
		{"http://1.2.3.4:3001/", "http://1.2.3.4:3001", false},
		{"http://[::1]:3001", "http://[::1]:3001", false},
		// invalid:
		{"", "", true},
		{"ftp://x:1", "", true},
		{"http://x:1\n", "", true},
		{"http://x\":1", "", true},
		{"http://x:1\\", "", true},
		{"http://x:1/path", "", true},
		{"http://x:1?q=1", "", true},
		{"http://x:1#frag", "", true},
		{"http://user@x:1", "", true},
		{"http://x:abc", "", true},
		{"http://x:0", "", true},
		{"http://x:99999", "", true},
		{"http:///", "", true},
		{"http://bad host:1", "", true},
	}
	for _, tc := range cases {
		got, err := sanitizeUpstream(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("sanitizeUpstream(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("sanitizeUpstream(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("sanitizeUpstream(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCreateRejectsYAMLInjection covers the regression Codex flagged: a
// caller with a valid token must not be able to inject YAML by smuggling
// newlines or quotes through the upstream field.
func TestCreateRejectsYAMLInjection(t *testing.T) {
	s, dir := newTestServer(t)
	w := do(s, "POST", "/v1/previews", "secret", linkReq{
		Project: "myrepo", Slug: "x",
		Upstream: "http://1.2.3.4:80\n          - url: \"http://attacker.example.com\"",
	})
	if w.Code != 400 {
		t.Fatalf("expected reject, got %d", w.Code)
	}
	// Verify no file was written.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("file written despite injection attempt: %v", entries)
	}
}

func TestListFilter(t *testing.T) {
	s, _ := newTestServer(t)
	for _, body := range []linkReq{
		{Project: "alpha", Slug: "main", Upstream: "http://x:1"},
		{Project: "alpha", Slug: "feature", Upstream: "http://x:2"},
		{Project: "beta", Slug: "main", Upstream: "http://y:1"},
	} {
		w := do(s, "POST", "/v1/previews", "secret", body)
		if w.Code != 200 {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
	}

	w := do(s, "GET", "/v1/previews?project=alpha", "secret", nil)
	var items []linkResp
	json.NewDecoder(w.Body).Decode(&items)
	if len(items) != 2 {
		t.Fatalf("want 2 alpha items, got %d", len(items))
	}
	for _, it := range items {
		if it.Project != "alpha" {
			t.Errorf("filter leaked: %s", it.Project)
		}
	}

	w = do(s, "GET", "/v1/previews", "secret", nil)
	json.NewDecoder(w.Body).Decode(&items)
	if len(items) != 3 {
		t.Fatalf("want 3 total, got %d", len(items))
	}
}

// TestLegacyFilenameCompat covers the upgrade path from the bash CLI which
// wrote wt-<project>-<slug>.yml files. The new API must be able to delete
// them and (when project filter is set) include them in list.
func TestLegacyFilenameCompat(t *testing.T) {
	s, dir := newTestServer(t)

	legacyPath := filepath.Join(dir, "wt-legacyproj-oldslug.yml")
	legacyYAML := "http:\n" +
		"  services:\n" +
		"    wt-legacyproj-oldslug:\n" +
		"      loadBalancer:\n" +
		"        servers:\n" +
		"          - url: \"http://10.0.0.99:9000\"\n"
	if err := os.WriteFile(legacyPath, []byte(legacyYAML), 0644); err != nil {
		t.Fatal(err)
	}

	t.Run("get reads legacy", func(t *testing.T) {
		w := do(s, "GET", "/v1/previews/legacyproj/oldslug", "secret", nil)
		if w.Code != 200 {
			t.Fatalf("got %d", w.Code)
		}
		var lr linkResp
		json.NewDecoder(w.Body).Decode(&lr)
		if lr.Upstream != "http://10.0.0.99:9000" {
			t.Fatalf("legacy upstream lost: %s", lr.Upstream)
		}
	})

	t.Run("list with filter includes legacy", func(t *testing.T) {
		w := do(s, "GET", "/v1/previews?project=legacyproj", "secret", nil)
		var items []linkResp
		json.NewDecoder(w.Body).Decode(&items)
		if len(items) != 1 || items[0].Slug != "oldslug" {
			t.Fatalf("legacy not listed: %+v", items)
		}
		if !items[0].Legacy {
			t.Errorf("expected Legacy=true, got %+v", items[0])
		}
	})

	t.Run("list without filter also includes legacy (best-effort split)", func(t *testing.T) {
		w := do(s, "GET", "/v1/previews", "secret", nil)
		var items []linkResp
		json.NewDecoder(w.Body).Decode(&items)
		var found *linkResp
		for i := range items {
			if items[i].Project == "legacyproj" && items[i].Slug == "oldslug" {
				found = &items[i]
			}
		}
		if found == nil {
			t.Fatalf("legacy not in unfiltered list: %+v", items)
		}
		if !found.Legacy {
			t.Errorf("expected Legacy=true on unfiltered match")
		}
	})

	t.Run("create cleans up legacy", func(t *testing.T) {
		w := do(s, "POST", "/v1/previews", "secret", linkReq{
			Project: "legacyproj", Slug: "oldslug", Upstream: "http://10.0.0.1:3001",
		})
		if w.Code != 200 {
			t.Fatalf("create: %d", w.Code)
		}
		if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
			t.Fatalf("legacy file still present after recreate: %v", err)
		}
		newPath := filepath.Join(dir, "wt-legacyproj__oldslug.yml")
		if _, err := os.Stat(newPath); err != nil {
			t.Fatalf("new file missing: %v", err)
		}
	})

	t.Run("delete removes legacy", func(t *testing.T) {
		// Re-create as legacy
		os.Remove(filepath.Join(dir, "wt-legacyproj__oldslug.yml"))
		os.WriteFile(legacyPath, []byte("http: {}\n"), 0644)
		w := do(s, "DELETE", "/v1/previews/legacyproj/oldslug", "secret", nil)
		if w.Code != 204 {
			t.Fatalf("got %d", w.Code)
		}
		if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
			t.Fatalf("legacy file still present: %v", err)
		}
	})
}

// TestListPrefersCanonicalOverLegacy ensures that when both filename
// formats coexist for the same project/slug, the new format wins in list
// output — matching handleGet's preference. ReadDir is filename-sorted so
// the legacy "-" file is visited before the new "__" file; a naive
// first-seen dedupe would surface stale legacy data.
func TestListPrefersCanonicalOverLegacy(t *testing.T) {
	s, dir := newTestServer(t)
	// Legacy entry first (older upstream).
	os.WriteFile(filepath.Join(dir, "wt-myproj-myslug.yml"),
		[]byte("services:\n    x:\n      loadBalancer:\n        servers:\n          - url: \"http://stale:1\"\n"), 0644)
	// Canonical/new entry (current upstream) created via API.
	w := do(s, "POST", "/v1/previews", "secret", linkReq{
		Project: "myproj", Slug: "myslug", Upstream: "http://current:2",
	})
	if w.Code != 200 {
		t.Fatalf("create: %d", w.Code)
	}
	// API's create cleans up the legacy file with same project/slug, but
	// to test list precedence in isolation, reinstate the legacy file.
	os.WriteFile(filepath.Join(dir, "wt-myproj-myslug.yml"),
		[]byte("services:\n    x:\n      loadBalancer:\n        servers:\n          - url: \"http://stale:1\"\n"), 0644)

	w = do(s, "GET", "/v1/previews?project=myproj", "secret", nil)
	var items []linkResp
	json.NewDecoder(w.Body).Decode(&items)
	if len(items) != 1 {
		t.Fatalf("want 1 deduped item, got %+v", items)
	}
	if items[0].Upstream != "http://current:2" {
		t.Errorf("want canonical upstream, got %q (legacy=%v)", items[0].Upstream, items[0].Legacy)
	}
	if items[0].Legacy {
		t.Errorf("expected Legacy=false for canonical winner")
	}

	// Same expectation in unfiltered list.
	w = do(s, "GET", "/v1/previews", "secret", nil)
	json.NewDecoder(w.Body).Decode(&items)
	if len(items) != 1 || items[0].Upstream != "http://current:2" || items[0].Legacy {
		t.Fatalf("unfiltered list did not prefer canonical: %+v", items)
	}
}

func TestRoutesYAMLContent(t *testing.T) {
	s, dir := newTestServer(t)
	do(s, "POST", "/v1/previews", "secret", linkReq{
		Project: "myrepo", Slug: "br-1", Upstream: "http://1.2.3.4:9999",
	})
	body, err := os.ReadFile(filepath.Join(dir, "wt-myrepo__br-1.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s2 := string(body)
	for _, want := range []string{
		"Host(`br-1.myrepo.preview.example.com`)",
		"http://1.2.3.4:9999",
		"entryPoints: [websecure]",
	} {
		if !strings.Contains(s2, want) {
			t.Errorf("yaml missing %q\n%s", want, s2)
		}
	}
}

func TestInstallScriptBakedIn(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(s, "GET", "/install.sh", "secret", nil)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `ENDPOINT="http://localhost:8080"`) {
		t.Errorf("endpoint not baked in:\n%s", body)
	}
	if !strings.Contains(body, `TOKEN="secret"`) {
		t.Errorf("token not baked in")
	}
}

func TestBinaryServeRejectsTraversal(t *testing.T) {
	s, _ := newTestServer(t)
	for _, path := range []string{
		"/bin/preview/../etc/passwd/x",
		"/bin/preview/linux/../../etc",
		"/bin/preview/LINUX/amd64",
	} {
		w := do(s, "GET", path, "secret", nil)
		if w.Code == 200 {
			t.Errorf("%s should not 200, got %d", path, w.Code)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(s, "PATCH", "/v1/previews", "secret", nil)
	if w.Code != 405 {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

// TestTraefikObjectNamesUnambiguous covers the collision Codex flagged:
// distinct (project, slug) pairs that produce different files must also
// produce different router/service keys inside the YAML, otherwise Traefik
// sees duplicate object names and silently drops one route.
func TestTraefikObjectNamesUnambiguous(t *testing.T) {
	s, dir := newTestServer(t)
	pairs := []struct {
		project, slug string
	}{
		{"foo-bar", "baz"},
		{"foo", "bar-baz"},
	}
	var names []string
	for _, p := range pairs {
		w := do(s, "POST", "/v1/previews", "secret", linkReq{
			Project: p.project, Slug: p.slug, Upstream: "http://1.2.3.4:80",
		})
		if w.Code != 200 {
			t.Fatalf("create %s/%s: %d %s", p.project, p.slug, w.Code, w.Body.String())
		}
		path := filepath.Join(dir, "wt-"+p.project+"__"+p.slug+".yml")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// Pull the router/service key out of the YAML — first match of
		// the pattern "wt-...:" inside the routers/services block.
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "wt-") && strings.HasSuffix(line, ":") {
				names = append(names, strings.TrimSuffix(line, ":"))
				break
			}
		}
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %v", names)
	}
	if names[0] == names[1] {
		t.Fatalf("Traefik object names collide for distinct (project, slug) pairs: %s", names[0])
	}
}

// TestListPropagatesReadError ensures the list path doesn't silently drop
// a route whose YAML file can't be read — operators must see the failure
// (HTTP 500) instead of an incomplete-but-200 response.
func TestListPropagatesReadError(t *testing.T) {
	s, dir := newTestServer(t)
	// Dangling symlink: ReadDir surfaces it as a non-directory entry, but
	// ReadFile follows the link and fails. Avoids needing chmod tricks
	// that don't work when the test runs as root.
	bad := filepath.Join(dir, "wt-x__y.yml")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), bad); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	w := do(s, "GET", "/v1/previews", "secret", nil)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "wt-x__y.yml") {
		t.Errorf("error body missing filename for diagnostics: %s", w.Body.String())
	}
}

// TestSkillFilesInSync guards against drift between the canonical
// skills/preview/SKILL.md (what users browse) and cmd/api/skill/SKILL.md
// (what the API embeds). The Makefile copies the former to the latter;
// editing one but forgetting to run `make sync-embed` would otherwise
// silently ship a stale skill via the install endpoint.
func TestSkillFilesInSync(t *testing.T) {
	embedded, err := os.ReadFile("skill/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded: %v", err)
	}
	canonical, err := os.ReadFile(filepath.Join("..", "..", "skills", "preview", "SKILL.md"))
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	if string(embedded) != string(canonical) {
		t.Fatalf("cmd/api/skill/SKILL.md is out of sync with skills/preview/SKILL.md — run `make sync-embed`")
	}
}

