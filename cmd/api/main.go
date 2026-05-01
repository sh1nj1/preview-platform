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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
}

type server struct {
	cfg config
}

func (s *server) routeFile(project, slug string) string {
	// "__" is a safe separator because slugs are validated to [a-z0-9-]+ only.
	return filepath.Join(s.cfg.DynamicDir, fmt.Sprintf("wt-%s__%s.yml", project, slug))
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
	if !strings.HasPrefix(req.Upstream, "http://") && !strings.HasPrefix(req.Upstream, "https://") {
		http.Error(w, "upstream must start with http:// or https://", 400)
		return
	}
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
	path := s.routeFile(project, slug)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "not found", 404)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}

var (
	routeFileRe = regexp.MustCompile(`^wt-([a-z0-9][a-z0-9-]*)__([a-z0-9][a-z0-9-]*)\.yml$`)
	urlLineRe   = regexp.MustCompile(`url:\s*"([^"]+)"`)
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
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := routeFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		project, slug := m[1], m[2]
		if filterProject != "" && project != filterProject {
			continue
		}
		body, err := os.ReadFile(filepath.Join(s.cfg.DynamicDir, e.Name()))
		if err != nil {
			continue
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
		})
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
