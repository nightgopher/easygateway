#!/usr/bin/env bash
# Install only the exact main revision whose full CI run passed, verify its
# artifact digest, and let systemd restart the service.
set -Eeuo pipefail
umask 077

API_BASE=${GATEWAY_API_BASE:-https://api.github.com/repos/nightgopher/litellm-gateway}
WORKFLOW=${GATEWAY_WORKFLOW:-ci.yml}
ARTIFACT_NAME=${GATEWAY_ARTIFACT_NAME:-gateway-linux-amd64}
TOKEN_FILE=${GATEWAY_GITHUB_TOKEN_FILE:-"$HOME/.config/litellm-gateway/github-token"}
BINARY=${GATEWAY_BINARY:-"$HOME/.llm-gateway/bin/gateway"}
SERVICE=${GATEWAY_SERVICE:-llm-gateway.service}
HEALTH_URL=${GATEWAY_HEALTH_URL:-http://127.0.0.1:4001/health}
STATE_DIR=${GATEWAY_UPDATE_STATE:-"$HOME/.local/state/litellm-gateway-update"}
API_HEADERS=(-H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28')

mkdir -p "$STATE_DIR" "$(dirname "$BINARY")"
exec 9>"$STATE_DIR/update.lock"
flock -n 9 || exit 0

WORK_DIR=$(mktemp -d "$STATE_DIR/run.XXXXXX")
trap 'rm -rf "$WORK_DIR"' EXIT
if [[ ! -r "$TOKEN_FILE" ]]; then
  echo "GitHub Actions read token file is missing: $TOKEN_FILE" >&2
  exit 1
fi
GITHUB_TOKEN=$(<"$TOKEN_FILE")
if [[ ! "$GITHUB_TOKEN" =~ ^(ghp_[A-Za-z0-9]+|github_pat_[A-Za-z0-9_]+)$ ]]; then
  echo "GitHub token file has an invalid format" >&2
  exit 1
fi
AUTH_CONFIG="$WORK_DIR/github-auth.conf"
printf 'header = "Authorization: Bearer %s"\n' "$GITHUB_TOKEN" > "$AUTH_CONFIG"
chmod 0600 "$AUTH_CONFIG"
unset GITHUB_TOKEN
CURL_AUTH_ARGS=(--config "$AUTH_CONFIG")

api_get() {
  curl --fail --silent --show-error --location --retry 2 \
    --connect-timeout 8 --max-time 45 "${API_HEADERS[@]}" "${CURL_AUTH_ARGS[@]}" "$1" -o "$2"
}

api_get "$API_BASE/commits/main" "$WORK_DIR/main.json"
REVISION=$(python3 - "$WORK_DIR/main.json" <<'PY'
import json, re, sys
sha = json.load(open(sys.argv[1], encoding="utf-8"))["sha"]
if not re.fullmatch(r"[0-9a-f]{40}", sha): raise SystemExit("invalid main SHA")
print(sha)
PY
)

if [[ -f "$STATE_DIR/failed-revision" ]] && [[ $(<"$STATE_DIR/failed-revision") == "$REVISION" ]]; then
  echo "Skipping previously failed deployment $REVISION; remove $STATE_DIR/failed-revision to retry"
  exit 0
fi
if [[ -f "$STATE_DIR/current-revision" ]] && [[ $(<"$STATE_DIR/current-revision") == "$REVISION" ]]; then
  echo "Already deployed $REVISION"
  exit 0
fi

api_get "$API_BASE/actions/workflows/$WORKFLOW/runs?branch=main&event=push&status=completed&per_page=1" "$WORK_DIR/runs.json"
read -r RUN_ID RUN_SHA RUN_CONCLUSION RUN_BRANCH RUN_EVENT < <(python3 - "$WORK_DIR/runs.json" <<'PY'
import json, sys
runs = json.load(open(sys.argv[1], encoding="utf-8")).get("workflow_runs", [])
if not runs:
    print("0 none none none none")
else:
    r = runs[0]
    print(r.get("id", 0), r.get("head_sha", "none"), r.get("conclusion", "none"), r.get("head_branch", "none"), r.get("event", "none"))
PY
)
if [[ "$RUN_SHA" != "$REVISION" || "$RUN_CONCLUSION" != success || "$RUN_BRANCH" != main || "$RUN_EVENT" != push || ! "$RUN_ID" =~ ^[1-9][0-9]*$ ]]; then
  echo "Waiting for successful main CI: main=$REVISION latest-run=$RUN_SHA/$RUN_CONCLUSION"
  exit 0
fi

api_get "$API_BASE/actions/runs/$RUN_ID/artifacts?per_page=100" "$WORK_DIR/artifacts.json"
read -r ARTIFACT_ID ARTIFACT_DIGEST < <(python3 - "$WORK_DIR/artifacts.json" "$ARTIFACT_NAME" "$RUN_ID" <<'PY'
import json, re, sys
items = json.load(open(sys.argv[1], encoding="utf-8")).get("artifacts", [])
items = [a for a in items if a.get("name") == sys.argv[2] and not a.get("expired") and a.get("workflow_run", {}).get("id") == int(sys.argv[3])]
if len(items) != 1: raise SystemExit("expected exactly one current Linux gateway artifact")
a = items[0]
digest = a.get("digest", "")
if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest): raise SystemExit("artifact is missing a SHA-256 digest")
if not 0 < a.get("size_in_bytes", 0) < 100_000_000: raise SystemExit("unexpected artifact size")
print(a["id"], digest.removeprefix("sha256:"))
PY
)

curl --fail --silent --show-error --location --retry 2 \
  --connect-timeout 8 --max-time 120 --max-filesize 100000000 \
  "${API_HEADERS[@]}" "${CURL_AUTH_ARGS[@]}" "$API_BASE/actions/artifacts/$ARTIFACT_ID/zip" -o "$WORK_DIR/artifact.zip"
printf '%s  %s\n' "$ARTIFACT_DIGEST" "$WORK_DIR/artifact.zip" | sha256sum --check --status
python3 - "$WORK_DIR/artifact.zip" "$WORK_DIR/gateway.new" <<'PY'
import stat, sys, zipfile
with zipfile.ZipFile(sys.argv[1]) as z:
    entries = [i for i in z.infolist() if not i.is_dir()]
    if len(entries) != 1 or entries[0].filename != "gateway-linux-amd64":
        raise SystemExit("artifact must contain only gateway-linux-amd64")
    if not 1024 <= entries[0].file_size < 100_000_000:
        raise SystemExit("unexpected gateway binary size")
    mode = entries[0].external_attr >> 16
    if stat.S_ISLNK(mode): raise SystemExit("binary artifact cannot be a symlink")
    raw = z.read(entries[0])
if len(raw) < 1024 or raw[:4] != b"\x7fELF" or int.from_bytes(raw[18:20], "little") != 62:
    raise SystemExit("artifact is not a Linux x86_64 ELF binary")
with open(sys.argv[2], "wb") as f: f.write(raw)
PY
chmod 0755 "$WORK_DIR/gateway.new"
BUILT_REVISION=$("$WORK_DIR/gateway.new" version)
if [[ "$BUILT_REVISION" != "$REVISION" ]]; then
  echo "artifact build revision does not match main; leaving the current binary untouched" >&2
  exit 1
fi

# A moving main branch or failed service must never replace the current binary.
api_get "$API_BASE/commits/main" "$WORK_DIR/main-final.json"
FINAL_REVISION=$(python3 - "$WORK_DIR/main-final.json" <<'PY'
import json, sys
print(json.load(open(sys.argv[1], encoding="utf-8"))["sha"])
PY
)
if [[ "$FINAL_REVISION" != "$REVISION" ]]; then
  echo "main moved during download; will retry the new revision on the next timer tick"
  exit 0
fi
if ! curl --fail --silent --show-error --max-time 4 "$HEALTH_URL" >/dev/null; then
  echo "gateway health check failed before deployment; leaving the current binary untouched" >&2
  exit 1
fi

CURRENT_PID=$(systemctl show --property=MainPID --value "$SERVICE")
[[ "$CURRENT_PID" =~ ^[1-9][0-9]*$ ]] || { echo "gateway service has no running main process" >&2; exit 1; }
if [[ ! -f "$BINARY" ]]; then
  echo "existing gateway binary is required for safe automatic upgrades" >&2
  exit 1
fi
cp -p "$BINARY" "$STATE_DIR/previous-binary"
install -m 0755 "$WORK_DIR/gateway.new" "$BINARY.next"
mv -f "$BINARY.next" "$BINARY"

if ! kill -TERM "$CURRENT_PID"; then
  install -m 0755 "$STATE_DIR/previous-binary" "$BINARY.rollback"
  mv -f "$BINARY.rollback" "$BINARY"
  echo "could not stop the old gateway; restored its binary" >&2
  exit 1
fi

HEALTHY=false
for _ in $(seq 1 45); do
  NEW_PID=$(systemctl show --property=MainPID --value "$SERVICE" 2>/dev/null || true)
  if [[ "$NEW_PID" =~ ^[1-9][0-9]*$ ]] && [[ "$NEW_PID" != "$CURRENT_PID" ]] && curl --fail --silent --max-time 2 "$HEALTH_URL" >/dev/null; then
    HEALTHY=true
    break
  fi
  sleep 1
done
if [[ "$HEALTHY" != true ]]; then
  install -m 0755 "$STATE_DIR/previous-binary" "$BINARY.rollback"
  mv -f "$BINARY.rollback" "$BINARY"
  NEW_PID=$(systemctl show --property=MainPID --value "$SERVICE" 2>/dev/null || true)
  if [[ "$NEW_PID" =~ ^[1-9][0-9]*$ ]]; then kill -TERM "$NEW_PID" 2>/dev/null || true; fi
  printf '%s\n' "$REVISION" > "$STATE_DIR/failed-revision"
  echo "new gateway failed its restart health check; restored the previous binary" >&2
  exit 1
fi

printf '%s\n' "$REVISION" > "$STATE_DIR/current-revision.next"
mv -f "$STATE_DIR/current-revision.next" "$STATE_DIR/current-revision"
rm -f "$STATE_DIR/failed-revision"
echo "Deployed and health-checked $REVISION"
