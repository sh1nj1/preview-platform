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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(s, "POST", "/v1/previews", "secret", tc.body)
			if w.Code != 400 {
				t.Fatalf("got %d, want 400", w.Code)
			}
		})
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

