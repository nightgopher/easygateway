#!/usr/bin/env bash
# One-time, unprivileged bootstrap for the q user on the Linux mini host.
set -Eeuo pipefail

REPOSITORY=nightgopher/easygateway
API="https://api.github.com/repos/$REPOSITORY"
API_HEADERS=(-H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28')
TOKEN_FILE="$HOME/.config/easygateway/github-token"

for command in curl python3 systemctl install flock sha256sum; do
  command -v "$command" >/dev/null || { echo "Missing required command: $command" >&2; exit 1; }
done
systemctl --user show-environment >/dev/null || { echo 'systemd user manager is unavailable' >&2; exit 1; }
install -d -m 0700 "$(dirname "$TOKEN_FILE")"
if [[ ! -s "$TOKEN_FILE" ]]; then
  echo "Create a read-only GitHub Actions token file before installing: $TOKEN_FILE" >&2
  exit 1
fi
chmod 0600 "$TOKEN_FILE"

REVISION=$(curl --fail --silent --show-error --location --retry 2 --connect-timeout 8 --max-time 45 \
  "${API_HEADERS[@]}" "$API/commits/main" |
  python3 -c 'import json,re,sys; s=json.load(sys.stdin)["sha"]; assert re.fullmatch(r"[0-9a-f]{40}",s); print(s)')
TEMP_DIR=$(mktemp -d)
trap 'rm -rf "$TEMP_DIR"' EXIT
for path in scripts/gateway-auto-update.sh deploy/systemd/gateway-auto-update.service deploy/systemd/gateway-auto-update.timer; do
  mkdir -p "$TEMP_DIR/$(dirname "$path")"
  curl --fail --silent --show-error --location --retry 2 --connect-timeout 8 --max-time 45 \
    "${API_HEADERS[@]}" "$API/contents/$path?ref=$REVISION" -o "$TEMP_DIR/content.json"
  python3 - "$TEMP_DIR/content.json" "$TEMP_DIR/$path" "$path" <<'PY'
import base64, hashlib, json, sys
item = json.load(open(sys.argv[1], encoding="utf-8"))
if item.get("path") != sys.argv[3] or item.get("encoding") != "base64":
    raise SystemExit("unexpected GitHub Contents API response")
raw = base64.b64decode("".join(item["content"].split()), validate=True)
blob = hashlib.sha1(f"blob {len(raw)}\0".encode() + raw).hexdigest()
if blob != item.get("sha"):
    raise SystemExit(f"GitHub file digest mismatch for {sys.argv[3]}")
with open(sys.argv[2], "wb") as f:
    f.write(raw)
PY
done

mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user" "$HOME/.config/easygateway" "$HOME/.local/state/easygateway-update"
install -m 0755 "$TEMP_DIR/scripts/gateway-auto-update.sh" "$HOME/.local/bin/gateway-auto-update"
install -m 0644 "$TEMP_DIR/deploy/systemd/gateway-auto-update.service" "$HOME/.config/systemd/user/gateway-auto-update.service"
install -m 0644 "$TEMP_DIR/deploy/systemd/gateway-auto-update.timer" "$HOME/.config/systemd/user/gateway-auto-update.timer"

CONFIG="$HOME/.config/easygateway/update.env"
if [[ ! -e "$CONFIG" ]]; then
  umask 077
  cat > "$CONFIG" <<CONFIG_EOF
GATEWAY_API_BASE=https://api.github.com/repos/nightgopher/easygateway
GATEWAY_GITHUB_TOKEN_FILE=$TOKEN_FILE
GATEWAY_BINARY=$HOME/.llm-gateway/bin/gateway
GATEWAY_SERVICE=llm-gateway.service
GATEWAY_HEALTH_URL=http://127.0.0.1:4001/health
GATEWAY_UPDATE_STATE=$HOME/.local/state/easygateway-update
CONFIG_EOF
fi

systemctl --user daemon-reload
systemctl --user enable --now gateway-auto-update.timer
printf 'Installed gateway updater for %s\n' "$REVISION"
printf 'Checking for the latest passing main build now...\n'
systemctl --user start gateway-auto-update.service
