// preview-api — small HTTP service that lets remote callers register/unregister
// preview routes by writing Traefik file-provider YAML into a watched directory.
//
// Endpoints:
//   POST   /v1/previews                       register / replace a route
//   GET    /v1/previews                       list (?project= filters)
//   GET    /v1/previews/{project}/{slug}      single route
//   DELETE /v1/previews/{project}/{slug}      remove route
//   GET    /install.sh                        bootstrap script (writes config + downloads binary)
//   GET    /bin/preview/{os}/{arch}           preview CLI binary
//   GET    /skills/preview                    Claude Code skill bundle (zip-style tar isn't needed; serves SKILL.md)
//   GET    /healthz
//
// All routes except /healthz require Authorization: Bearer <token>.
package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

//go:embed all:dist
var binaries embed.FS

//go:embed install.sh.tmpl
var installTmpl string

//go:embed skill/SKILL.md
var skillContent string

type config struct {
	ListenAddr   string
	Token        string
	Domain       string
	DynamicDir   string
	PublicAPIURL string
}

func loadConfigFromEnv() (config, error) {
	c := config{
		ListenAddr:   envOr("LISTEN_ADDR", ":8080"),
		Token:        os.Getenv("PREVIEW_API_TOKEN"),
		Domain:       os.Getenv("PREVIEW_DOMAIN"),
		DynamicDir:   envOr("DYNAMIC_DIR", "/dynamic"),
		PublicAPIURL: os.Getenv("PREVIEW_PUBLIC_API_URL"),
	}
	if c.Token == "" {
		return c, errors.New("PREVIEW_API_TOKEN required")
	}
	if c.Domain == "" {
		return c, errors.New("PREVIEW_DOMAIN required")
	}
	if c.PublicAPIURL == "" {
		c.PublicAPIURL = "https://api." + c.Domain
	}
	return c, nil
}

func envOr(k, dflt string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return dflt
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

func validSlug(s string) bool { return len(s) <= 63 && slugRe.MatchString(s) }

// sanitizeUpstream parses the requested upstream URL and rebuilds it from
// trusted components, defending against YAML injection (newlines, embedded
// quotes) by anyone with a valid API token. We only allow scheme://host[:port]
// — no path, query, fragment, or userinfo, since the Traefik file provider
// expects a base URL for the load balancer.
func sanitizeUpstream(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("empty")
	}
	if strings.ContainsAny(raw, "\n\r\t\"\\") {
		return "", errors.New("contains forbidden characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("scheme must be http or https")
	}
	if u.User != nil {
		return "", errors.New("userinfo not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("query or fragment not allowed")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("path not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("missing host")
	}
	if ip := net.ParseIP(host); ip == nil {
		// Hostname must be a DNS label (no embedded special chars).
		for _, r := range host {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '.' || r == '-'
			if !ok {
				return "", errors.New("invalid host character")
			}
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid port")
		}
	}
	hostPart := host
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		hostPart = "[" + host + "]"
	}
	out := u.Scheme + "://" + hostPart
	if port != "" {
		out += ":" + port
	}
	return out, nil
}

type linkReq struct {
	Project  string `json:"project"`
	Slug     string `json:"slug"`
	Upstream string `json:"upstream"`
}

type linkResp struct {
	URL      string `json:"url"`
	Project  string `json:"project"`
	Slug     string `json:"slug"`
	Upstream string `json:"upstream"`
	// Legacy is true when this entry was discovered via the bash CLI's
	// filename format (wt-<project>-<slug>.yml). Project/slug parsing is
	// best-effort because hyphens make the split ambiguous; operators
	// should use this flag to identify routes that need cleanup.
	Legacy bool `json:"legacy,omitempty"`
}

type server struct {
	cfg config
}

func (s *server) routeFile(project, slug string) string {
	// "__" is a safe separator because slugs are validated to [a-z0-9-]+ only.
	return filepath.Join(s.cfg.DynamicDir, fmt.Sprintf("wt-%s__%s.yml", project, slug))
}

// legacyRouteFile is the filename written by the previous bash CLI
// (wt-<project>-<slug>.yml). We never write this format, but read/delete
// honor it for upgrade compatibility with deployments that have files
// left over from the bash workflow.
func (s *server) legacyRouteFile(project, slug string) string {
	return filepath.Join(s.cfg.DynamicDir, fmt.Sprintf("wt-%s-%s.yml", project, slug))
}

func (s *server) hostFor(project, slug string) string {
	return fmt.Sprintf("%s.%s.%s", slug, project, s.cfg.Domain)
}

func (s *server) urlFor(project, slug string) string {
	return "https://" + s.hostFor(project, slug)
}

const routeYAMLTmpl = `http:
  routers:
    %[1]s:
      rule: "Host(` + "`%[2]s`" + `)"
      entryPoints: [websecure]
      service: %[1]s
      tls: {}
  services:
    %[1]s:
      loadBalancer:
        servers:
          - url: "%[3]s"
`

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req linkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), 400)
		return
	}
	if !validSlug(req.Project) || !validSlug(req.Slug) {
		http.Error(w, "invalid project or slug (lowercase alphanumeric + hyphen)", 400)
		return
	}
	clean, err := sanitizeUpstream(req.Upstream)
	if err != nil {
		http.Error(w, "invalid upstream: "+err.Error(), 400)
		return
	}
	req.Upstream = clean
	name := fmt.Sprintf("wt-%s-%s", req.Project, req.Slug)
	yaml := fmt.Sprintf(routeYAMLTmpl, name, s.hostFor(req.Project, req.Slug), req.Upstream)
	if err := os.MkdirAll(s.cfg.DynamicDir, 0755); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := os.WriteFile(s.routeFile(req.Project, req.Slug), []byte(yaml), 0644); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Drop any legacy-format file with the same project/slug so Traefik
	// doesn't load conflicting route definitions during a partial upgrade.
	if err := os.Remove(s.legacyRouteFile(req.Project, req.Slug)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("warn: removing legacy route file: %v", err)
	}
	writeJSON(w, 200, linkResp{
		URL:      s.urlFor(req.Project, req.Slug),
		Project:  req.Project,
		Slug:     req.Slug,
		Upstream: req.Upstream,
	})
}

func (s *server) handleDelete(w http.ResponseWriter, project, slug string) {
	if !validSlug(project) || !validSlug(slug) {
		http.Error(w, "invalid project or slug", 400)
		return
	}
	// Try both the new and legacy filenames so previews registered by
	// the old bash CLI are also deletable through this endpoint.
	removed := false
	for _, path := range []string{s.routeFile(project, slug), s.legacyRouteFile(project, slug)} {
		err := os.Remove(path)
		if err == nil {
			removed = true
			continue
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		http.Error(w, err.Error(), 500)
		return
	}
	if !removed {
		http.Error(w, "not found", 404)
		return
	}
	w.WriteHeader(204)
}

var (
	routeFileRe       = regexp.MustCompile(`^wt-([a-z0-9][a-z0-9-]*)__([a-z0-9][a-z0-9-]*)\.yml$`)
	legacyRouteFileRe = regexp.MustCompile(`^wt-([a-z0-9][a-z0-9-]*)\.yml$`)
	urlLineRe         = regexp.MustCompile(`url:\s*"([^"]+)"`)
)

func (s *server) listAll(filterProject string) ([]linkResp, error) {
	entries, err := os.ReadDir(s.cfg.DynamicDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []linkResp
	seen := map[string]bool{}
	add := func(project, slug, name string, legacy bool) {
		key := project + "/" + slug
		if seen[key] {
			return
		}
		seen[key] = true
		body, err := os.ReadFile(filepath.Join(s.cfg.DynamicDir, name))
		if err != nil {
			return
		}
		upstream := ""
		if m := urlLineRe.FindStringSubmatch(string(body)); m != nil {
			upstream = m[1]
		}
		out = append(out, linkResp{
			URL:      s.urlFor(project, slug),
			Project:  project,
			Slug:     slug,
			Upstream: upstream,
			Legacy:   legacy,
		})
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// First pass: new format (unambiguous and canonical).
		if m := routeFileRe.FindStringSubmatch(e.Name()); m != nil {
			project, slug := m[1], m[2]
			if filterProject != "" && project != filterProject {
				continue
			}
			add(project, slug, e.Name(), false)
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Second pass: legacy format. Skipped for any project/slug already
		// covered by the new format above so the canonical file always wins
		// in mixed upgrade states (matches handleGet's preference).
		if routeFileRe.MatchString(e.Name()) {
			continue
		}
		// Legacy format (wt-<project>-<slug>.yml). The split is ambiguous
		// when project names contain hyphens. With a project filter we get
		// an exact prefix; without one, we fall back to "first hyphen as
		// separator" — wrong for hyphenated project names but better than
		// silently hiding routes during an upgrade. Callers can identify
		// best-effort entries via the Legacy flag.
		if m := legacyRouteFileRe.FindStringSubmatch(e.Name()); m != nil {
			body := m[1]
			if filterProject != "" {
				prefix := filterProject + "-"
				if !strings.HasPrefix(body, prefix) {
					continue
				}
				slug := strings.TrimPrefix(body, prefix)
				if validSlug(slug) {
					add(filterProject, slug, e.Name(), true)
				}
				continue
			}
			// Unfiltered: best-effort split on first hyphen.
			idx := strings.Index(body, "-")
			if idx <= 0 || idx == len(body)-1 {
				continue
			}
			project, slug := body[:idx], body[idx+1:]
			if validSlug(project) && validSlug(slug) {
				add(project, slug, e.Name(), true)
			}
		}
	}
	return out, nil
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	items, err := s.listAll(project)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if items == nil {
		items = []linkResp{}
	}
	writeJSON(w, 200, items)
}

func (s *server) handleGet(w http.ResponseWriter, project, slug string) {
	if !validSlug(project) || !validSlug(slug) {
		http.Error(w, "invalid project or slug", 400)
		return
	}
	body, err := os.ReadFile(s.routeFile(project, slug))
	if errors.Is(err, fs.ErrNotExist) {
		body, err = os.ReadFile(s.legacyRouteFile(project, slug))
	}
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	upstream := ""
	if m := urlLineRe.FindStringSubmatch(string(body)); m != nil {
		upstream = m[1]
	}
	writeJSON(w, 200, linkResp{
		URL:      s.urlFor(project, slug),
		Project:  project,
		Slug:     slug,
		Upstream: upstream,
	})
}

func (s *server) handleInstall(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.New("install").Parse(installTmpl)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	tmpl.Execute(w, map[string]string{
		"Endpoint": s.cfg.PublicAPIURL,
		"Token":    s.cfg.Token,
	})
}

func (s *server) handleBinary(w http.ResponseWriter, goos, goarch string) {
	if !isSafeIdent(goos) || !isSafeIdent(goarch) {
		http.Error(w, "bad os/arch", 400)
		return
	}
	name := fmt.Sprintf("preview-%s-%s", goos, goarch)
	f, err := binaries.Open("dist/" + name)
	if err != nil {
		http.Error(w, "binary not built for "+goos+"/"+goarch, 404)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	io.Copy(w, f)
}

func (s *server) handleSkill(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	io.WriteString(w, skillContent)
}

func isSafeIdent(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") || h[7:] != s.cfg.Token {
			http.Error(w, "unauthorized", 401)
			return
		}
		next(w, r)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/install.sh", s.authed(s.handleInstall))
	mux.HandleFunc("/skills/preview", s.authed(s.handleSkill))

	mux.HandleFunc("/bin/preview/", s.authed(func(w http.ResponseWriter, r *http.Request) {
		// /bin/preview/{os}/{arch}
		rest := strings.TrimPrefix(r.URL.Path, "/bin/preview/")
		parts := strings.Split(rest, "/")
		if len(parts) != 2 {
			http.Error(w, "expected /bin/preview/{os}/{arch}", 400)
			return
		}
		s.handleBinary(w, parts[0], parts[1])
	}))

	mux.HandleFunc("/v1/previews", s.authed(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			s.handleCreate(w, r)
		case "GET":
			s.handleList(w, r)
		default:
			http.Error(w, "method not allowed", 405)
		}
	}))
	mux.HandleFunc("/v1/previews/", s.authed(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1/previews/")
		parts := strings.Split(rest, "/")
		if len(parts) != 2 {
			http.Error(w, "expected /v1/previews/{project}/{slug}", 400)
			return
		}
		switch r.Method {
		case "GET":
			s.handleGet(w, parts[0], parts[1])
		case "DELETE":
			s.handleDelete(w, parts[0], parts[1])
		default:
			http.Error(w, "method not allowed", 405)
		}
	}))
	return logMW(mux)
}

func logMW(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		h.ServeHTTP(w, r)
	})
}

func main() {
	cfg, err := loadConfigFromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	s := &server{cfg: cfg}
	log.Printf("preview-api listening on %s (domain=%s, dynamic=%s)", cfg.ListenAddr, cfg.Domain, cfg.DynamicDir)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, s.routes()))
}
