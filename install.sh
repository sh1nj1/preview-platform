#!/usr/bin/env bash
# Bootstrap the preview platform on a fresh Ubuntu/Debian server.
#
# Run from the cloned repo root:
#   sudo ./install.sh

set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/srv/preview-platform}"

if [ "$(id -u)" -ne 0 ]; then
  echo "==> Re-running with sudo"
  exec sudo -E "$0" "$@"
fi

REAL_USER="${SUDO_USER:-$USER}"

echo "==> Installing Docker if missing"
if ! command -v docker >/dev/null 2>&1; then
  curl -fsSL https://get.docker.com | sh
  usermod -aG docker "$REAL_USER" || true
  echo "    (you may need to log out and back in for group membership)"
fi

echo "==> Creating ${INSTALL_DIR}"
mkdir -p "${INSTALL_DIR}"/{dynamic,letsencrypt,examples,cmd,skills}
touch "${INSTALL_DIR}/letsencrypt/acme.json"
chmod 600 "${INSTALL_DIR}/letsencrypt/acme.json"

echo "==> Copying files"
cp ./docker-compose.yml  "${INSTALL_DIR}/"
cp ./Dockerfile.api      "${INSTALL_DIR}/"
cp ./Makefile            "${INSTALL_DIR}/"
cp ./go.mod              "${INSTALL_DIR}/"
cp -R ./cmd/.            "${INSTALL_DIR}/cmd/"
cp -R ./skills/.         "${INSTALL_DIR}/skills/"
cp -R ./examples/.       "${INSTALL_DIR}/examples/"
[ -f "${INSTALL_DIR}/.env" ] || cp ./.env.example "${INSTALL_DIR}/.env"

# preview-api container writes to /dynamic; allow that.
chmod 0775 "${INSTALL_DIR}/dynamic"

cat <<EOF

==> Done.

Next steps:
  1. Edit ${INSTALL_DIR}/.env
       - PREVIEW_DOMAIN, ACME_EMAIL, AWS_*, DASHBOARD_AUTH
       - PREVIEW_API_TOKEN (run: openssl rand -hex 32)
  2. Confirm Route53 has wildcard A record (covers traefik.* and api.*):
       *.<your-domain>  →  $(hostname -I | awk '{print $1}')
  3. cd ${INSTALL_DIR} && docker compose up -d --build
  4. Watch cert issuance:  docker compose logs -f traefik | grep -i acme
  5. Visit  https://traefik.<your-domain>  (BasicAuth)

Developers install the 'preview' CLI on their machine with:
  curl -fsSL -H "Authorization: Bearer <PREVIEW_API_TOKEN>" \\
    https://api.<your-domain>/install.sh | bash
  # add --with-skill to also install the Claude Code skill

EOF
