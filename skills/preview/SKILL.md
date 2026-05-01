---
name: preview
description: Use this skill when the user wants to expose a local dev server through the team's preview-platform (e.g. "share this branch", "give me a preview link", "register this worktree", "publish to preview"). Calls the `preview` CLI to register a route on the central preview server and returns the HTTPS URL. Requires the `preview` CLI to be installed and configured (~/.config/preview/config).
---

# preview-platform skill

The user has a self-hosted preview server (Traefik + a small Go API) that turns a local
dev server into a public(-ish) HTTPS URL like `https://<branch>.<repo>.<domain>`.

## When to use

Trigger when the user asks to:
- "share this branch / preview / link"
- "give me a URL for this dev server"
- "publish this to preview"
- "register this worktree"
- "stop sharing / unlink the preview"

## How to use

The CLI is `preview` (in `~/.local/bin` or wherever `--prefix` placed it). It must
be run from inside a git working tree — branch and repo name are auto-detected.

### Create a preview link

```bash
preview link
```

Output (stdout = URL only, stderr = human-readable):
```
https://feature-auth.myrepo.preview.example.com
```

It also writes `.preview.env` with `PORT=<auto-allocated>` to the current
directory. The user starts their dev server using that port:

```bash
source .preview.env && npm run dev   # or rails s -p $PORT, etc.
```

The dev server **must bind to 0.0.0.0** (not localhost) for Traefik to reach it.

### Common flags

```bash
preview link --port 4000              # use a specific port
preview link --slug my-demo           # override slug (default: branch name)
preview link --upstream http://10.0.0.5:9000   # full override (e.g. remote)
```

### Other commands

```bash
preview list                # active previews for this repo
preview list --all          # all projects
preview url                 # print URL for current branch
preview unlink              # remove the route
```

### Configuration

CLI reads `~/.config/preview/config`:
```
endpoint=https://api.preview.example.com
token=<bearer token>
```
Or via env: `PREVIEW_API`, `PREVIEW_API_TOKEN`.

If the user hasn't installed yet, point them at:
```
curl -fsSL -H "Authorization: Bearer <TOKEN>" https://api.<domain>/install.sh | bash
```

## Tips

- After `preview link`, **always remind** the user to start their dev server with
  `source .preview.env && <dev command>` and ensure it binds to `0.0.0.0`.
- For Vite/Next dev servers, the user may need `server.allowedHosts` or a
  `--host 0.0.0.0` flag.
- If the user is on a laptop behind NAT, an upstream IP from the local network
  may not be reachable by the preview server — Tailscale is the typical fix.
  The CLI auto-detects Tailscale IPs (`tailscale ip -4`) when present.
