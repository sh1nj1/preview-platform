// preview — register/unregister dev servers with the preview platform API.
//
//	preview link [--port N] [--upstream URL] [--slug NAME]
//	preview unlink [--slug NAME]
//	preview list
//	preview url [--slug NAME]
//
// Configuration is read from ~/.config/preview/config (key=value), with these
// keys: endpoint, token. Environment variables PREVIEW_API and
// PREVIEW_API_TOKEN override the file.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const usage = `preview — register dev servers with the preview platform.

Commands:
  preview link    [--port N] [--upstream URL] [--slug NAME]
  preview unlink  [--slug NAME]
  preview list
  preview url     [--slug NAME]

Config file: ~/.config/preview/config (endpoint=, token=)
Env override: PREVIEW_API, PREVIEW_API_TOKEN, PREVIEW_HOST_IP,
              PREVIEW_PORT_START (3001), PREVIEW_PORT_END (3099)
`

type config struct {
	Endpoint string
	Token    string
}

func loadConfig() (*config, error) {
	c := &config{}
	if usr, err := user.Current(); err == nil {
		path := filepath.Join(usr.HomeDir, ".config", "preview", "config")
		if data, err := os.ReadFile(path); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				k, v, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				v = strings.Trim(strings.TrimSpace(v), `"'`)
				switch strings.TrimSpace(k) {
				case "endpoint":
					c.Endpoint = v
				case "token":
					c.Token = v
				}
			}
		}
	}
	if v := os.Getenv("PREVIEW_API"); v != "" {
		c.Endpoint = v
	}
	if v := os.Getenv("PREVIEW_API_TOKEN"); v != "" {
		c.Token = v
	}
	if c.Endpoint == "" {
		return nil, fmt.Errorf("no API endpoint configured (run install or set PREVIEW_API)")
	}
	if c.Token == "" {
		return nil, fmt.Errorf("no API token configured (set PREVIEW_API_TOKEN)")
	}
	return c, nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9-]`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = slugRe.ReplaceAllString(s, "")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

func currentProject() (string, error) {
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not in a git repo")
	}
	return slugify(filepath.Base(root)), nil
}

func currentSlug() (string, error) {
	branch, err := git("branch", "--show-current")
	if err != nil || branch == "" {
		return "", fmt.Errorf("cannot detect branch (detached HEAD?); use --slug")
	}
	return slugify(branch), nil
}

func detectIP() string {
	if v := os.Getenv("PREVIEW_HOST_IP"); v != "" {
		return v
	}
	if out, err := exec.Command("tailscale", "ip", "-4").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			ip := strings.TrimSpace(line)
			if ip != "" {
				return ip
			}
		}
	}
	return firstNonLoopbackIPv4()
}

// firstNonLoopbackIPv4 enumerates local interfaces and returns the first
// non-loopback, non-link-local IPv4 address belonging to an "up" interface.
// Avoids any external network probe so it works on LAN-only or
// egress-restricted hosts.
func firstNonLoopbackIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

func envInt(k string, dflt int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return dflt
}

// freePort returns a port that is unused on every local interface, since the
// dev server the user starts will typically bind 0.0.0.0 — checking only the
// loopback would miss collisions on a LAN/Tailscale interface.
func freePort() (int, error) {
	start, end := envInt("PREVIEW_PORT_START", 3001), envInt("PREVIEW_PORT_END", 3099)
	for p := start; p <= end; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", p))
		if err == nil {
			l.Close()
			return p, nil
		}
	}
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return 0, fmt.Errorf("no free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
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
}

type listItem struct {
	Project  string `json:"project"`
	Slug     string `json:"slug"`
	URL      string `json:"url"`
	Upstream string `json:"upstream"`
}

func apiCall(c *config, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(c.Endpoint, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func decodeOrErr(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func cmdLink(args []string) error {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	port := fs.Int("port", 0, "port (0 = auto)")
	upstreamFlag := fs.String("upstream", "", "upstream URL override (http://host:port)")
	slugFlag := fs.String("slug", "", "slug override")
	fs.Parse(args)

	c, err := loadConfig()
	if err != nil {
		return err
	}
	project, err := currentProject()
	if err != nil {
		return err
	}
	slug := slugify(*slugFlag)
	if slug == "" {
		slug, err = currentSlug()
		if err != nil {
			return err
		}
	}

	upstream := *upstreamFlag
	var localPort int
	if upstream == "" {
		ip := detectIP()
		if ip == "" {
			return fmt.Errorf("could not detect local IP; pass --upstream")
		}
		localPort = *port
		if localPort == 0 {
			localPort, err = freePort()
			if err != nil {
				return err
			}
		}
		upstream = fmt.Sprintf("http://%s:%d", ip, localPort)
	} else if u, err := url.Parse(upstream); err == nil {
		if p, _ := strconv.Atoi(u.Port()); p > 0 {
			localPort = p
		}
	}

	resp, err := apiCall(c, "POST", "/v1/previews", linkReq{Project: project, Slug: slug, Upstream: upstream})
	if err != nil {
		return err
	}
	var lr linkResp
	if err := decodeOrErr(resp, &lr); err != nil {
		return err
	}

	if localPort > 0 {
		os.WriteFile(".preview.env", []byte(fmt.Sprintf("PORT=%d\n", localPort)), 0644)
	}

	fmt.Println(lr.URL)
	fmt.Fprintf(os.Stderr, "✓ linked\n  upstream: %s\n", upstream)
	if localPort > 0 {
		fmt.Fprintf(os.Stderr, "  env:      .preview.env (PORT=%d)\n\n  source .preview.env && <your dev server>\n", localPort)
	}
	return nil
}

func cmdUnlink(args []string) error {
	fs := flag.NewFlagSet("unlink", flag.ExitOnError)
	slugFlag := fs.String("slug", "", "slug override")
	fs.Parse(args)

	c, err := loadConfig()
	if err != nil {
		return err
	}
	project, err := currentProject()
	if err != nil {
		return err
	}
	slug := slugify(*slugFlag)
	if slug == "" {
		slug, err = currentSlug()
		if err != nil {
			return err
		}
	}
	resp, err := apiCall(c, "DELETE", "/v1/previews/"+project+"/"+slug, nil)
	if err != nil {
		return err
	}
	if err := decodeOrErr(resp, nil); err != nil {
		return err
	}
	os.Remove(".preview.env")
	fmt.Fprintf(os.Stderr, "✓ unlinked %s\n", slug)
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	all := fs.Bool("all", false, "list all projects (default: current project only)")
	fs.Parse(args)

	c, err := loadConfig()
	if err != nil {
		return err
	}
	path := "/v1/previews"
	if !*all {
		project, err := currentProject()
		if err != nil {
			return err
		}
		path += "?project=" + url.QueryEscape(project)
	}
	resp, err := apiCall(c, "GET", path, nil)
	if err != nil {
		return err
	}
	var items []listItem
	if err := decodeOrErr(resp, &items); err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "no active previews")
		return nil
	}
	for _, it := range items {
		fmt.Printf("%-40s  %s\n", it.URL, it.Upstream)
	}
	return nil
}

func cmdURL(args []string) error {
	fs := flag.NewFlagSet("url", flag.ExitOnError)
	slugFlag := fs.String("slug", "", "slug override")
	fs.Parse(args)
	c, err := loadConfig()
	if err != nil {
		return err
	}
	project, err := currentProject()
	if err != nil {
		return err
	}
	slug := slugify(*slugFlag)
	if slug == "" {
		slug, err = currentSlug()
		if err != nil {
			return err
		}
	}
	resp, err := apiCall(c, "GET", "/v1/previews/"+project+"/"+slug, nil)
	if err != nil {
		return err
	}
	var it listItem
	if err := decodeOrErr(resp, &it); err != nil {
		return err
	}
	fmt.Println(it.URL)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "link":
		err = cmdLink(os.Args[2:])
	case "unlink", "rm":
		err = cmdUnlink(os.Args[2:])
	case "list", "ls":
		err = cmdList(os.Args[2:])
	case "url":
		err = cmdURL(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "version":
		fmt.Println("preview cli (built from sh1nj1/preview-platform)")
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "preview: %v\n", err)
		os.Exit(1)
	}
}
