#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "${SCRIPT_DIR}/.." && pwd)
GATEWAY_DIR="${REPO_ROOT}/go-gateway"
RUNTIME_BASE="${HOME}/.local/share/hwj-runtimes"
LAUNCH_AGENT_PLIST="${HOME}/Library/LaunchAgents/local.go-gateway.plist"
LAUNCH_AGENT_LABEL="local.go-gateway"
SERVICE_DOMAIN="gui/$(id -u)"
SERVICE_TARGET="${SERVICE_DOMAIN}/${LAUNCH_AGENT_LABEL}"
SERVICE_PORT=4001

if [[ "$(uname -s)" != "Darwin" ]]; then
    echo "This deploy script manages the macOS local.go-gateway LaunchAgent." >&2
    exit 1
fi

for command_name in go python3 launchctl plutil lsof ps shasum; do
    if ! command -v "${command_name}" >/dev/null 2>&1; then
        echo "Required command not found: ${command_name}" >&2
        exit 1
    fi
done

if [[ ! -f "${LAUNCH_AGENT_PLIST}" ]]; then
    echo "LaunchAgent plist not found: ${LAUNCH_AGENT_PLIST}" >&2
    exit 1
fi

WORKING_DIR=$(python3 - "${LAUNCH_AGENT_PLIST}" <<'PY'
import plistlib
import sys

with open(sys.argv[1], "rb") as source:
    print(plistlib.load(source).get("WorkingDirectory", ""))
PY
)
CURRENT_BINARY=$(python3 - "${LAUNCH_AGENT_PLIST}" <<'PY'
import plistlib
import sys

with open(sys.argv[1], "rb") as source:
    arguments = plistlib.load(source).get("ProgramArguments", [])
if not arguments:
    raise SystemExit("LaunchAgent has no ProgramArguments")
print(arguments[0])
PY
)

if [[ -z "${WORKING_DIR}" || ! -d "${WORKING_DIR}" || ! -f "${WORKING_DIR}/.env" ]]; then
    echo "LaunchAgent WorkingDirectory or its .env is missing; refusing to deploy." >&2
    exit 1
fi
if [[ ! -x "${CURRENT_BINARY}" ]]; then
    echo "Current LaunchAgent binary is missing or not executable: ${CURRENT_BINARY}" >&2
    exit 1
fi

LISTENER_PID=$(lsof -nP -iTCP:"${SERVICE_PORT}" -sTCP:LISTEN -t 2>/dev/null | head -n 1 || true)
if [[ -n "${LISTENER_PID}" ]]; then
    LISTENER_COMMAND=$(ps -p "${LISTENER_PID}" -o command= | sed 's/^[[:space:]]*//')
    if [[ "${LISTENER_COMMAND}" != "${CURRENT_BINARY}" ]]; then
        echo "Port ${SERVICE_PORT} is owned by an unexpected process; refusing to stop it." >&2
        exit 1
    fi
fi

SOURCE_COMMIT=$(git -C "${REPO_ROOT}" rev-parse --verify HEAD)
SOURCE_BRANCH=$(git -C "${REPO_ROOT}" branch --show-current)
if git -C "${REPO_ROOT}" rev-parse --verify --quiet refs/remotes/origin/main >/dev/null; then
    if ! git -C "${REPO_ROOT}" merge-base --is-ancestor refs/remotes/origin/main HEAD; then
        echo "Current source does not include the locally fetched origin/main. Update it before deploying." >&2
        exit 1
    fi
fi
SHORT_COMMIT=${SOURCE_COMMIT:0:8}
BUILD_STAMP=$(date -u +%Y%m%d-%H%M%S)
mkdir -p "${RUNTIME_BASE}"
CANDIDATE_DIR=$(mktemp -d "${RUNTIME_BASE}/gateway-local-${BUILD_STAMP}-${SHORT_COMMIT}-XXXXXX")
CANDIDATE_BINARY="${CANDIDATE_DIR}/go-llm-gateway"

echo "Building candidate from ${SOURCE_BRANCH:-detached}@${SOURCE_COMMIT}"
(cd "${GATEWAY_DIR}" && go build -o "${CANDIDATE_BINARY}" .)

if git -C "${REPO_ROOT}" status --porcelain --untracked-files=normal | grep -q .; then
    SOURCE_STATE=modified
else
    SOURCE_STATE=clean
fi

cat > "${CANDIDATE_DIR}/deploy-info.txt" <<EOF
source_branch=${SOURCE_BRANCH:-detached}
source_commit=${SOURCE_COMMIT}
source_state=${SOURCE_STATE}
built_at_utc=${BUILD_STAMP}
binary_sha256=$(shasum -a 256 "${CANDIDATE_BINARY}" | awk '{print $1}')
tracked_source_diff_sha256=$(git -C "${REPO_ROOT}" diff --binary HEAD | shasum -a 256 | awk '{print $1}')
deploy_script_sha256=$(shasum -a 256 "${SCRIPT_DIR}/deploy-local-gateway.sh" | awk '{print $1}')
EOF

echo "Preflighting candidate against isolated storage on an unused local port."
python3 - "${CANDIDATE_BINARY}" "${WORKING_DIR}" "${LAUNCH_AGENT_PLIST}" <<'PY'
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urljoin
from urllib.request import Request, urlopen

binary, working_dir, launch_plist = sys.argv[1:]
import plistlib
with open(launch_plist, "rb") as source:
    skills_repo = plistlib.load(source).get("EnvironmentVariables", {}).get("SKILLS_REPO_PATH", "")
temp_data = tempfile.mkdtemp(prefix="gateway-candidate-preflight-")
sock = socket.socket()
sock.bind(("127.0.0.1", 0))
port = sock.getsockname()[1]
sock.close()
base = f"http://127.0.0.1:{port}/"
token = "local-gateway-preflight-token"
env = {
    "PATH": os.environ.get("PATH", "/usr/bin:/bin:/usr/sbin:/sbin"),
    "HOME": os.path.expanduser("~"),
    "PORT": str(port),
    "PI_GO_DATA_DIR": temp_data,
    "EASYGATEWAY_MASTER_KEY": token,
    "ADMIN_TOKEN": token,
}
if skills_repo:
    env["SKILLS_REPO_PATH"] = skills_repo
process = subprocess.Popen(
    [binary], cwd=working_dir, env=env,
    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
)

def request(path, authenticated=False):
    headers = {"Authorization": f"Bearer {token}"} if authenticated else {}
    req = Request(urljoin(base, path), headers=headers)
    try:
        with urlopen(req, timeout=2) as response:
            return response.status, response.read()
    except HTTPError as error:
        return error.code, error.read()

try:
    for _ in range(80):
        if process.poll() is not None:
            raise RuntimeError(f"candidate exited with status {process.returncode}")
        try:
            request("/health")
            break
        except (URLError, TimeoutError, OSError):
            time.sleep(0.25)
    else:
        raise RuntimeError("candidate did not become healthy")

    required_cache_fields = {
        "cache_read_input_tokens", "cache_creation_input_tokens",
        "cache_input_tokens", "cache_usage_requests", "cache_hit_rate",
    }
    for path in ("/admin/dashboard", "/admin/stats"):
        status, body = request(path, authenticated=True)
        payload = json.loads(body)
        summary = payload.get("summary", {})
        missing = sorted(required_cache_fields - set(summary))
        if status != 200 or missing:
            raise RuntimeError(f"{path} failed: status={status}, missing={missing}")

    for path in ("/admin/memories?limit=1", "/admin/assistant/feedback?limit=1", "/admin/skills"):
        status, _ = request(path, authenticated=True)
        if status != 200:
            raise RuntimeError(f"{path} failed: status={status}")

    status, prompt_body = request("/admin/assistant/prompt", authenticated=True)
    if status == 503 and "助理未启用" in prompt_body.decode(errors="replace"):
        pass
    elif status != 200:
        raise RuntimeError(f"/admin/assistant/prompt failed: status={status}")

    # Verify the native route without sending any inference request. This also
    # permits upgrading old runtime YAML before migrating its Jev definitions.
    for authenticated, expected in ((False, 401), (True, 400)):
        headers = {"Content-Type": "application/json"}
        if authenticated:
            headers["Authorization"] = f"Bearer {token}"
        req = Request(urljoin(base, "/v1/systemone"), data=b"{}", headers=headers)
        try:
            status = urlopen(req, timeout=2).status
        except HTTPError as error:
            status = error.code
        if status != expected:
            raise RuntimeError(f"System One preflight failed: status={status}, expected={expected}")

    for path in ("/", "/dashboard", "/dashboard/models", "/assets/index.js", "/favicon.ico"):
        status, _ = request(path, authenticated=True)
        if status != 404:
            raise RuntimeError(f"removed browser route {path} still responds: status={status}")
        status, _ = request(path)
        if status != 401:
            raise RuntimeError(f"removed browser route {path} bypasses authentication: status={status}")
except Exception as error:
    print(f"Candidate preflight failed: {error}", file=sys.stderr)
    raise SystemExit(1)
finally:
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()
    shutil.rmtree(temp_data, ignore_errors=True)

print("Candidate preflight passed: Admin API, memory, assistant, and API-only routes.")
PY

PLIST_BACKUP="${CANDIDATE_DIR}/local.go-gateway.plist.before"
cp -p "${LAUNCH_AGENT_PLIST}" "${PLIST_BACKUP}"
PLIST_UPDATED=0
DEPLOY_SUCCEEDED=0

restore_service() {
    status=$?
    trap - EXIT
    if [[ "${status}" -ne 0 && "${PLIST_UPDATED}" -eq 1 && "${DEPLOY_SUCCEEDED}" -eq 0 ]]; then
        echo "Deployment failed; restoring the previous LaunchAgent and binary." >&2
        launchctl bootout "${SERVICE_TARGET}" >/dev/null 2>&1 || true
        cp -p "${PLIST_BACKUP}" "${LAUNCH_AGENT_PLIST}"
        plutil -lint "${LAUNCH_AGENT_PLIST}" >/dev/null 2>&1 || true
        launchctl bootstrap "${SERVICE_DOMAIN}" "${LAUNCH_AGENT_PLIST}" >/dev/null 2>&1 || true
        python3 - "${CURRENT_BINARY}" "${SERVICE_PORT}" <<'PY'
import subprocess
import sys
import time
from urllib.request import urlopen

expected, port = sys.argv[1], int(sys.argv[2])
for _ in range(80):
    try:
        with urlopen(f"http://127.0.0.1:{port}/health", timeout=1) as response:
            if response.status == 200:
                break
    except Exception:
        pass
    time.sleep(0.25)
else:
    print("Previous gateway did not become healthy after rollback.", file=sys.stderr)
    raise SystemExit(1)

try:
    pids = subprocess.check_output(
        ["lsof", "-nP", f"-iTCP:{port}", "-sTCP:LISTEN", "-t"],
        text=True, stderr=subprocess.DEVNULL,
    ).splitlines()
    command = subprocess.check_output(
        ["ps", "-p", pids[0], "-o", "command="], text=True,
    ).strip()
    if command != expected:
        print("Rollback health check reached an unexpected process.", file=sys.stderr)
        raise SystemExit(1)
except (IndexError, subprocess.CalledProcessError):
    print("Could not verify the restored gateway process.", file=sys.stderr)
    raise SystemExit(1)
PY
    fi
    exit "${status}"
}
trap restore_service EXIT

python3 - "${LAUNCH_AGENT_PLIST}" "${CANDIDATE_BINARY}" <<'PY'
import os
import plistlib
import sys

path, binary = sys.argv[1:]
with open(path, "rb") as source:
    plist = plistlib.load(source)
arguments = plist.get("ProgramArguments", [])
if not arguments:
    raise SystemExit("LaunchAgent has no ProgramArguments")
arguments[0] = binary
plist["ProgramArguments"] = arguments
temp_path = path + ".tmp"
mode = os.stat(path).st_mode & 0o777
with open(temp_path, "wb") as destination:
    plistlib.dump(plist, destination, fmt=plistlib.FMT_XML, sort_keys=False)
os.chmod(temp_path, mode)
os.replace(temp_path, path)
PY
PLIST_UPDATED=1
plutil -lint "${LAUNCH_AGENT_PLIST}" >/dev/null

echo "Switching the LaunchAgent to the verified candidate."
launchctl bootout "${SERVICE_TARGET}" >/dev/null 2>&1 || true
python3 - "${SERVICE_PORT}" <<'PY'
import subprocess
import sys
import time

port = int(sys.argv[1])
for _ in range(80):
    result = subprocess.run(
        ["lsof", "-nP", f"-iTCP:{port}", "-sTCP:LISTEN", "-t"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    if result.returncode != 0:
        break
    time.sleep(0.25)
else:
    raise SystemExit(f"Port {port} did not become free after stopping the old gateway")
PY
launchctl bootstrap "${SERVICE_DOMAIN}" "${LAUNCH_AGENT_PLIST}"

python3 - "${CANDIDATE_BINARY}" "${SERVICE_PORT}" <<'PY'
import subprocess
import sys
import time
from urllib.request import urlopen

expected, port = sys.argv[1], int(sys.argv[2])
for _ in range(80):
    try:
        with urlopen(f"http://127.0.0.1:{port}/health", timeout=1) as response:
            healthy = response.status == 200
        pids = subprocess.check_output(
            ["lsof", "-nP", f"-iTCP:{port}", "-sTCP:LISTEN", "-t"],
            text=True, stderr=subprocess.DEVNULL,
        ).splitlines()
        command = subprocess.check_output(
            ["ps", "-p", pids[0], "-o", "command="], text=True,
        ).strip()
        if healthy and command == expected:
            break
    except Exception:
        pass
    time.sleep(0.25)
else:
    raise SystemExit("Candidate did not become healthy on the production port")
PY

DEPLOY_SUCCEEDED=1
echo "Deployed ${CANDIDATE_BINARY}"
echo "Build record: ${CANDIDATE_DIR}/deploy-info.txt"
