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
	"bufio"
	"bytes"
	"crypto/tls"
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

const defaultHostsFile = "/etc/hosts"
const hostsMarker = "# preview"

// hostsAdd appends "<ip> <hostname> # preview" to the hosts file if not already present.
// If the file is not writable, it retries via "sudo tee -a".
func hostsAdd(hostsFile, hostname, ip string) error {
	data, err := os.ReadFile(hostsFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	entry := ip + " " + hostname + " " + hostsMarker
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil
		}
	}
	f, err := os.OpenFile(hostsFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		if os.IsPermission(err) {
			return hostsAddSudo(hostsFile, entry)
		}
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, entry)
	return err
}

func hostsAddSudo(hostsFile, entry string) error {
	cmd := exec.Command("sudo", "tee", "-a", hostsFile)
	cmd.Stdin = strings.NewReader(entry + "\n")
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// hostsRemove removes lines matching "<any-ip> <hostname> # preview" from the hosts file.
// If the file is not writable, it retries via "sudo tee".
func hostsRemove(hostsFile, hostname string) error {
	data, err := os.ReadFile(hostsFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	var kept []string
	for _, line := range lines {
		if !hostsLineMatchesHost(line, hostname) {
			kept = append(kept, line)
		}
	}
	if len(kept) == len(lines) {
		return nil
	}
	content := []byte(strings.Join(kept, "\n"))
	err = os.WriteFile(hostsFile, content, 0644)
	if err != nil && os.IsPermission(err) {
		return hostsWriteSudo(hostsFile, content)
	}
	return err
}

func hostsWriteSudo(hostsFile string, content []byte) error {
	cmd := exec.Command("sudo", "tee", hostsFile)
	cmd.Stdin = bytes.NewReader(content)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// hostsLineMatchesHost reports whether line is a preview hosts entry for hostname.
// Expected format: "<ip> <hostname> # preview"
func hostsLineMatchesHost(line, hostname string) bool {
	fields := strings.Fields(line)
	return len(fields) >= 4 && fields[1] == hostname && fields[2] == "#" && fields[3] == "preview"
}

const usage = `preview — register dev servers with the preview platform.

Commands:
  preview link    [--port N] [--upstream URL] [--slug NAME] [--insecure]
  preview unlink  [--slug NAME] [--insecure]
  preview list    [--insecure]
  preview url     [--slug NAME] [--insecure]

Config file: ~/.config/preview/config (endpoint=, token=, insecure=true)
Env override: PREVIEW_API, PREVIEW_API_TOKEN, PREVIEW_INSECURE,
              PREVIEW_HOST_IP, PREVIEW_PORT_START (3001), PREVIEW_PORT_END (3099)
`

type config struct {
	Endpoint string
	Token    string
	Insecure bool
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
				case "insecure":
					c.Insecure = v == "true" || v == "1" || v == "yes"
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
	if v := os.Getenv("PREVIEW_INSECURE"); v == "1" || v == "true" {
		c.Insecure = true
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

// firstNonLoopbackIPv4 picks the local IPv4 address that the preview server
// is most likely to be able to reach. It tries, in order:
//
//  1. The interface that owns the default route (Linux: /proc/net/route).
//     This is what the kernel itself would pick to send outbound traffic
//     and matches "the IP that's actually reachable from elsewhere on the
//     network" for the typical case.
//  2. Enumerate all "up" interfaces and prefer one whose name doesn't look
//     like a virtual NIC (docker*, br-*, veth*, virbr*, vboxnet*, tun*,
//     tap*, wg*, tailscale*). Falls back to any virtual NIC IP only if
//     that's the only thing available.
//
// No external network probe is performed, so this works on LAN-only and
// egress-restricted hosts.
func firstNonLoopbackIPv4() string {
	if iface := defaultRouteInterface(); iface != "" {
		if ip := ipv4OfInterface(iface); ip != "" {
			return ip
		}
	}
	return preferredInterfaceIPv4()
}

// defaultRouteInterface parses /proc/net/route and returns the name of the
// interface owning the default (0.0.0.0/0) route. Linux-only; returns ""
// elsewhere or when no default route is configured.
func defaultRouteInterface() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(scanner.Text())
		// fields: Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		if len(fields) >= 8 && fields[1] == "00000000" {
			return fields[0]
		}
	}
	return ""
}

func ipv4OfInterface(name string) string {
	iface, err := net.InterfaceByName(name)
	if err != nil || iface.Flags&net.FlagUp == 0 {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
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
	return ""
}

var virtualIfacePrefixes = []string{
	"docker", "br-", "virbr", "vboxnet", "veth",
	"tun", "tap", "wg", "tailscale", "zt",
}

func looksVirtual(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func preferredInterfaceIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var virtual string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		ip := ipv4OfInterface(iface.Name)
		if ip == "" {
			continue
		}
		if looksVirtual(iface.Name) {
			if virtual == "" {
				virtual = ip
			}
			continue
		}
		return ip
	}
	return virtual
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
	client := &http.Client{Timeout: 30 * time.Second}
	if c.Insecure {
		fmt.Fprintln(os.Stderr, "warning: TLS certificate verification disabled (--insecure)")
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		}
	}
	return client.Do(req)
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
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	writeHosts := fs.Bool("write-hosts", false, "add preview hostname to /etc/hosts (may require sudo)")
	hostsIP := fs.String("hosts-ip", "127.0.0.1", "IP address written to /etc/hosts (with --write-hosts)")
	fs.Parse(args)

	c, err := loadConfig()
	if err != nil {
		return err
	}
	if *insecure {
		c.Insecure = true
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
		// The route is already registered server-side, so a write failure
		// here would leave the user with a successful link but no
		// .preview.env to source. Surface the error and tell them to
		// roll back the registration manually.
		if err := os.WriteFile(".preview.env", []byte(fmt.Sprintf("PORT=%d\n", localPort)), 0644); err != nil {
			return fmt.Errorf("link registered (%s) but writing .preview.env failed: %w\n  run `preview unlink` to roll back", lr.URL, err)
		}
	}

	fmt.Println(lr.URL)
	fmt.Fprintf(os.Stderr, "✓ linked\n  upstream: %s\n", upstream)
	if localPort > 0 {
		fmt.Fprintf(os.Stderr, "  env:      .preview.env (PORT=%d)\n\n  source .preview.env && <your dev server>\n", localPort)
	}
	if *writeHosts {
		u, _ := url.Parse(lr.URL)
		hostname := u.Hostname()
		if err := hostsAdd(defaultHostsFile, hostname, *hostsIP); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: /etc/hosts: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "  hosts:    %s %s\n", *hostsIP, hostname)
		}
	}
	return nil
}

func cmdUnlink(args []string) error {
	fs := flag.NewFlagSet("unlink", flag.ExitOnError)
	slugFlag := fs.String("slug", "", "slug override")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	writeHosts := fs.Bool("write-hosts", false, "remove preview hostname from /etc/hosts (may require sudo)")
	fs.Parse(args)

	c, err := loadConfig()
	if err != nil {
		return err
	}
	if *insecure {
		c.Insecure = true
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

	// Resolve the preview hostname before deleting the route.
	var previewHostname string
	if *writeHosts {
		if resp, err := apiCall(c, "GET", "/v1/previews/"+project+"/"+slug, nil); err == nil {
			var it listItem
			if decodeOrErr(resp, &it) == nil {
				if u, err := url.Parse(it.URL); err == nil {
					previewHostname = u.Hostname()
				}
			}
		}
		if previewHostname == "" {
			fmt.Fprintf(os.Stderr, "  warn: could not resolve preview URL; remove /etc/hosts entry manually\n")
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

	if *writeHosts && previewHostname != "" {
		if err := hostsRemove(defaultHostsFile, previewHostname); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: /etc/hosts: %v\n", err)
		}
	}

	fmt.Fprintf(os.Stderr, "✓ unlinked %s\n", slug)
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	all := fs.Bool("all", false, "list all projects (default: current project only)")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	fs.Parse(args)

	c, err := loadConfig()
	if err != nil {
		return err
	}
	if *insecure {
		c.Insecure = true
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
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	fs.Parse(args)
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if *insecure {
		c.Insecure = true
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
