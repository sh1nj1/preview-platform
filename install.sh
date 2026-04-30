#!/usr/bin/env bash
# Bootstrap the preview platform on a fresh Ubuntu/Debian server.
#
# Run from the cloned repo root:
#   sudo ./install.sh

set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/srv/preview-platform}"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"

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
mkdir -p "${INSTALL_DIR}"/{dynamic,letsencrypt,examples,bin}
touch "${INSTALL_DIR}/letsencrypt/acme.json"
chmod 600 "${INSTALL_DIR}/letsencrypt/acme.json"

echo "==> Copying files"
cp ./docker-compose.yml         "${INSTALL_DIR}/"
cp -R ./examples/.              "${INSTALL_DIR}/examples/"
cp ./bin/preview                "${INSTALL_DIR}/bin/"
[ -f "${INSTALL_DIR}/.env" ] || cp ./.env.example "${INSTALL_DIR}/.env"

# The dynamic dir must be writable by whoever runs `preview link` from worktrees.
# Default: the user who invoked sudo. Adjust the group as needed.
chown -R "$REAL_USER":"$REAL_USER" "${INSTALL_DIR}/dynamic"
chmod 0775 "${INSTALL_DIR}/dynamic"

echo "==> Installing 'preview' CLI to ${BIN_DIR}"
install -m 0755 ./bin/preview "${BIN_DIR}/preview"

cat <<EOF

==> Done.

Next steps:
  1. Edit ${INSTALL_DIR}/.env (domain, AWS credentials, dashboard auth).
  2. Confirm Route53 has wildcard A record:  *.<your-domain>  →  $(hostname -I | awk '{print $1}')
  3. cd ${INSTALL_DIR} && docker compose up -d
  4. Watch cert issuance:  docker compose logs -f traefik | grep -i acme
  5. Visit  https://traefik.<your-domain>  (BasicAuth)

For developers using 'preview link' from their workstations:
  - Set in their shell profile:
      export PREVIEW_DOMAIN=<your-domain>
      export PREVIEW_DYNAMIC_DIR=${INSTALL_DIR}/dynamic   # local OR sshfs-mounted
  - Ensure they can write to ${INSTALL_DIR}/dynamic (group access or sshfs mount).

EOF
