#!/usr/bin/env bash
# Creates an IAM user with minimal Route 53 permissions for Traefik DNS-01
# ACME challenge, then generates access keys and prints .env-ready output.
#
# Usage:
#   ./setup-iam.sh [--user NAME] [--zone-id ZONE_ID] [--policy-name NAME]
#
# Options:
#   --user         IAM username to create (default: preview-platform-traefik)
#   --zone-id      Route 53 Hosted Zone ID to scope permissions (optional;
#                  omit to allow all zones)
#   --policy-name  IAM policy name (default: preview-platform-route53)
#
# Prerequisites:
#   - aws CLI installed and configured with permissions to manage IAM
#   - jq installed

set -euo pipefail

# ── Defaults ──────────────────────────────────────────────────────────────────
IAM_USER="${IAM_USER:-preview-platform-traefik}"
POLICY_NAME="${POLICY_NAME:-preview-platform-route53}"
ZONE_ID=""

# ── Argument parsing ───────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case $1 in
    --user)        IAM_USER="$2";    shift 2 ;;
    --zone-id)     ZONE_ID="$2";    shift 2 ;;
    --policy-name) POLICY_NAME="$2"; shift 2 ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

# ── Dependency check ──────────────────────────────────────────────────────────
for cmd in aws jq; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Error: '$cmd' is required but not installed." >&2
    exit 1
  fi
done

# ── Build Route 53 resource ARN ───────────────────────────────────────────────
if [[ -n "$ZONE_ID" ]]; then
  ROUTE53_RESOURCE="arn:aws:route53:::hostedzone/${ZONE_ID}"
else
  ROUTE53_RESOURCE="arn:aws:route53:::hostedzone/*"
fi

AWS_ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)"
POLICY_ARN="arn:aws:iam::${AWS_ACCOUNT_ID}:policy/${POLICY_NAME}"

echo "==> Account  : ${AWS_ACCOUNT_ID}"
echo "==> IAM user : ${IAM_USER}"
echo "==> Policy   : ${POLICY_NAME}"
if [[ -n "$ZONE_ID" ]]; then
  echo "==> Zone     : ${ZONE_ID} (scoped)"
else
  echo "==> Zone     : all zones"
fi
echo

# ── IAM Policy ────────────────────────────────────────────────────────────────
POLICY_DOC="$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "route53:ChangeResourceRecordSets",
        "route53:ListResourceRecordSets"
      ],
      "Resource": "${ROUTE53_RESOURCE}"
    },
    {
      "Effect": "Allow",
      "Action": "route53:ListHostedZonesByName",
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": "route53:GetChange",
      "Resource": "arn:aws:route53:::change/*"
    }
  ]
}
EOF
)"

if aws iam get-policy --policy-arn "$POLICY_ARN" >/dev/null 2>&1; then
  echo "==> Policy already exists, skipping creation: ${POLICY_ARN}"
else
  echo "==> Creating IAM policy: ${POLICY_NAME}"
  aws iam create-policy \
    --policy-name "$POLICY_NAME" \
    --policy-document "$POLICY_DOC" \
    --description "Minimal Route 53 access for Traefik DNS-01 ACME challenge" \
    --output table
fi

# ── IAM User ──────────────────────────────────────────────────────────────────
if aws iam get-user --user-name "$IAM_USER" >/dev/null 2>&1; then
  echo "==> IAM user already exists, skipping creation: ${IAM_USER}"
else
  echo "==> Creating IAM user: ${IAM_USER}"
  aws iam create-user --user-name "$IAM_USER" --output table
fi

# ── Attach policy to user ─────────────────────────────────────────────────────
echo "==> Attaching policy to user"
aws iam attach-user-policy \
  --user-name "$IAM_USER" \
  --policy-arn "$POLICY_ARN"

# ── Create access key ─────────────────────────────────────────────────────────
# Warn if user already has 2 keys (AWS limit)
KEY_COUNT="$(aws iam list-access-keys --user-name "$IAM_USER" \
  --query 'length(AccessKeyMetadata)' --output text)"
if [[ "$KEY_COUNT" -ge 2 ]]; then
  echo "Error: ${IAM_USER} already has 2 access keys (AWS limit)." >&2
  echo "       Delete an existing key and re-run, or use an existing key." >&2
  exit 1
fi

echo "==> Creating access key"
KEY_JSON="$(aws iam create-access-key --user-name "$IAM_USER")"

ACCESS_KEY_ID="$(echo "$KEY_JSON"     | jq -r '.AccessKey.AccessKeyId')"
SECRET_ACCESS_KEY="$(echo "$KEY_JSON" | jq -r '.AccessKey.SecretAccessKey')"

# ── Output ─────────────────────────────────────────────────────────────────────
echo
echo "==> Done. Add the following to your .env file:"
echo
echo "AWS_ACCESS_KEY_ID=${ACCESS_KEY_ID}"
echo "AWS_SECRET_ACCESS_KEY=${SECRET_ACCESS_KEY}"
[[ -n "$ZONE_ID" ]] && echo "AWS_HOSTED_ZONE_ID=${ZONE_ID}"
echo
echo "IMPORTANT: The secret access key is shown only once. Store it now."
