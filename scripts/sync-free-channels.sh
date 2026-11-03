#!/usr/bin/env bash
# 每日免费渠道同步：比对匿名免费上游的实时清单，修剪 providers.yaml 中已失效的
# 免费模型，并探活主力模型，输出对比报告。
#
# 数据源：
#   - OpenCode 公共车道 GET /inference/v1/models（Bearer public，实时清单）
#   - Kilo GET /api/gateway/models（isFree=true 的免费集合）
#   - dsh-our-free-model 的 registry.json（仅上下文参考，可能过期，不影响修剪判定）
#
# 自动应用范围（窄）：只修剪 kilo 这类 allow_no_key Provider 中已从上游免费清单
# 消失、且不被 chains 引用的模型；改前备份、经网关 POST /admin/config/check 校验、
# POST /admin/config/reload 热重载，失败自动回滚备份。上游新增模型不自动加入。
# 其余一切变化只写报告。OpenCode 匿名车道只探测不接入（上游按出口 IP 结算免费
# 额度，且校验官方客户端请求形态，见 go-gateway/README.md「免费渠道维护与每日同步」）。
#
# 用法:
#   sync-free-channels.sh [--dry-run] [--report-only] [-endpoint URL] [-config PATH]
#
# 环境变量（均可覆盖默认值）:
#   EASYGATEWAY_ENDPOINT     网关地址（默认 http://127.0.0.1:4001）
#   EASYGATEWAY_CONFIG       providers.yaml 路径（默认 Mac 本机运行时配置）
#   EASYGATEWAY_MASTER_KEY   管理凭据；缺省时从配置同目录 .env 推导
#   EASYGATEWAY_SYNC_LOG     报告追加路径（默认 ~/Library/Logs/easygateway-free-sync.log）
#   OPENCODE_MODELS_URL / KILO_MODELS_URL / DSH_REGISTRY_URL   上游地址覆盖（测试用）
#
# 退出码: 0 正常结束（含只报告与无变化）；1 参数错误、前置条件失败或修补失败
# （修补失败时已还原备份，配置与运行时保持旧状态）。
set -Eeuo pipefail

ENDPOINT="${EASYGATEWAY_ENDPOINT:-http://127.0.0.1:4001}"
CONFIG_PATH="${EASYGATEWAY_CONFIG:-$HOME/.local/share/hwj-runtimes/gateway-local-restored-20261022-175336/providers.yaml}"
LOG_PATH="${EASYGATEWAY_SYNC_LOG:-$HOME/Library/Logs/easygateway-free-sync.log}"
OPENCODE_MODELS_URL="${OPENCODE_MODELS_URL:-https://opencode.ai/inference/v1/models}"
KILO_MODELS_URL="${KILO_MODELS_URL:-https://api.kilo.ai/api/gateway/models}"
DSH_REGISTRY_URL="${DSH_REGISTRY_URL:-https://raw.githubusercontent.com/Ebony-Vinyl/dsh-our-free-model/main/src/providers/registry.json}"
DSH_REGISTRY_FALLBACK_URL="https://cdn.jsdelivr.net/gh/Ebony-Vinyl/dsh-our-free-model@main/src/providers/registry.json"
# 探活目标：coding 链的稳定供给（keyed 与 DeepV 代码集成）+ 匿名免费兜底
PROBE_MODELS=("glm-5.3-flash" "deepseek-flash" "deepseek-ai/DeepSeek-V4.1-Flash" "kilo-auto/free")
OPENCODE_PROBE_MODEL="deepseek-v4.1-flash-free"
BACKUP_KEEP=5
CURL_TIMEOUT=30

DRY_RUN=0
REPORT_ONLY=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --report-only) REPORT_ONLY=1; shift ;;
    -endpoint) ENDPOINT="$2"; shift 2 ;;
    -config) CONFIG_PATH="$2"; shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

log() { printf '%s\n' "$*"; }
die() { printf '%s\n' "错误: $*" >&2; exit 1; }

fetch_url() { # $1=url $2=失败说明（可空则允许失败）
  local url="$1" what="${2:-}" body
  if ! body=$(curl -sf --max-time "$CURL_TIMEOUT" "$url" 2>/dev/null); then
    [ -n "$what" ] && die "获取 $what 失败: $url"
    printf '%s' ""
    return 1
  fi
  printf '%s' "$body"
}

fetch_dsh_registry() {
  local body
  body=$(fetch_url "$DSH_REGISTRY_URL" "") && printf '%s' "$body" && return 0
  body=$(fetch_url "$DSH_REGISTRY_FALLBACK_URL" "") && printf '%s' "$body" && return 0
  return 1
}

# ── 管理凭据 ──────────────────────────────────────────────────────────────────
find_admin_token() {
  local candidate candidates=()
  if [ -n "${EASYGATEWAY_MASTER_KEY:-}" ]; then candidates+=("$EASYGATEWAY_MASTER_KEY"); fi
  local env_file config_dir key_name value
  config_dir=$(dirname "$CONFIG_PATH")
  env_file="$config_dir/.env"
  if [ -f "$env_file" ]; then
    while IFS= read -r key_name; do
      value=$(grep "^${key_name}=" "$env_file" | tail -1 | cut -d= -f2- | tr -d '"' || true)
      [ -n "$value" ] && candidates+=("$value")
    done <<'EOF'
EASYGATEWAY_MASTER_KEY
ADMIN_TOKEN
LITELLM_MASTER_KEY
EOF
  fi
  for candidate in "${candidates[@]:-}"; do
    [ -z "$candidate" ] && continue
    if curl -sf --max-time 10 -H "Authorization: Bearer $candidate" "$ENDPOINT/admin/health" >/dev/null 2>&1; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  return 1
}

admin_post() { # $1=path $2=json-body-or-empty
  local path="$1" body="${2:-}"
  if [ -n "$body" ]; then
    curl -sf --max-time "$CURL_TIMEOUT" -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
      -H "Content-Type: application/json" -d "$body" "$ENDPOINT$path"
  else
    curl -sf --max-time "$CURL_TIMEOUT" -X POST -H "Authorization: Bearer $ADMIN_TOKEN" "$ENDPOINT$path"
  fi
}

admin_get() {
  curl -sf --max-time "$CURL_TIMEOUT" -H "Authorization: Bearer $ADMIN_TOKEN" "$ENDPOINT$1"
}

# ── 上游清单 ──────────────────────────────────────────────────────────────────
log "== 免费渠道同步 $(date '+%Y-%m-%d %H:%M:%S') =="
log "网关: $ENDPOINT"
log "配置: $CONFIG_PATH"

[ -f "$CONFIG_PATH" ] || die "配置文件不存在: $CONFIG_PATH（用 -config 指定路径）"

OPENCODE_LIST=$(fetch_url "$OPENCODE_MODELS_URL" "OpenCode 公共车道模型清单") || OPENCODE_LIST=""
KILO_LIST=$(fetch_url "$KILO_MODELS_URL" "Kilo 模型清单") || KILO_LIST=""
DSH_REGISTRY=$(fetch_dsh_registry || true)

[ -n "$KILO_LIST" ] || die "Kilo 清单不可用：为避免误删，本次不执行修剪（仅报告）"

# ── 比对与修剪计算（python3 内嵌：yaml 文本级手术，注释与键序保留）──────────
# 输入: 配置文件、kilo 实时免费集合；输出: 修剪候选（不含 chains 引用的模型）。
PRUNE_PLAN=$(CONFIG_FILE="$CONFIG_PATH" KILO_LIST="$KILO_LIST" python3 <<'PYEOF'
import hashlib, json, os, re, sys

config_path = os.environ["CONFIG_FILE"]
kilo_raw = os.environ["KILO_LIST"]
with open(config_path, "r", encoding="utf-8") as f:
    lines = f.readlines()

# Kilo 实时免费集合（isFree=true 的 id；无 isFree 字段的结构按 data/models 数组兼容）
try:
    kilo_doc = json.loads(kilo_raw)
except Exception as exc:
    print(f"KILLO_PARSE_ERROR {exc}", file=sys.stderr)
    sys.exit(3)
items = kilo_doc if isinstance(kilo_doc, list) else None
if items is None:
    items = kilo_doc.get("data") or kilo_doc.get("models") or []
live_free = set()
for it in items:
    if not isinstance(it, dict):
        continue
    mid = it.get("id") or it.get("name") or ""
    if mid and it.get("isFree") is True:
        live_free.add(mid)

# 定位 kilo provider 块与其 models 条目
prov_re = re.compile(r"^  - name: (\S+)\s*$")
id_re = re.compile(r"^(\s+)- id: (\S+)\s*$")
prov_start = prov_end = None
for i, line in enumerate(lines):
    m = prov_re.match(line)
    if m:
        if m.group(1) == "kilo":
            prov_start = i
        elif prov_start is not None and prov_end is None:
            prov_end = i
if prov_start is None:
    print("NO_KILO_PROVIDER")
    sys.exit(0)
if prov_end is None:
    prov_end = len(lines)
# chains 起点之后不算 provider 内容
for i in range(prov_start, prov_end):
    if lines[i].startswith("chains:"):
        prov_end = i
        break

entries = []  # ((id, id_line_idx), last_line_idx_inclusive)
current = None
current_last = -1
for i in range(prov_start + 1, prov_end):
    m = id_re.match(lines[i])
    if m and len(m.group(1)) == 6:  # models 条目缩进为 6 空格
        if current:
            entries.append((current, current_last))
        current = (m.group(2), i)
        current_last = i
        # 条目体：紧随其后的更深缩进非空行（capabilities 等）
        j = i + 1
        while j < prov_end and lines[j].strip() != "":
            s = lines[j]
            if len(s) - len(s.lstrip(" ")) < 8:
                break
            current_last = j
            j += 1
if current:
    entries.append((current, current_last))

# chains 引用的模型不可修剪
chain_refs = set()
in_chains = False
for line in lines:
    if line.startswith("chains:"):
        in_chains = True
        continue
    if in_chains:
        m = re.match(r"^    - (\S+)\s*$", line)
        if m:
            chain_refs.add(m.group(1))

remove = [e for e in entries if e[0][0] not in live_free]
blocked = [e[0][0] for e in remove if e[0][0] in chain_refs]
removable = [e for e in remove if e[0][0] not in chain_refs]
if blocked:
    print("CHAIN_BLOCKED\t" + ",".join(blocked))
if removable:
    print("PRUNE\t" + ",".join(e[0][0] for e in removable))
    print("RANGES\t" + ";".join(f"{e[0][1]}-{e[1]}={e[0][0]}" for e in removable))
else:
    print("PRUNE\t(none)")
PYEOF
) || PRUNE_STATUS=$?

if [ "${PRUNE_STATUS:-0}" -eq 3 ]; then
  die "Kilo 清单解析失败：本次不执行修剪"
fi

PRUNE_IDS=$(printf '%s' "$PRUNE_PLAN" | grep '^PRUNE' | head -1 | cut -f2)
CHAIN_BLOCKED=$(printf '%s' "$PRUNE_PLAN" | grep '^CHAIN_BLOCKED' | cut -f2 || true)
RANGES=$(printf '%s' "$PRUNE_PLAN" | grep '^RANGES' | cut -f2 || true)

# ── 上游新增（只报告）───────────────────────────────────────────────────────
UPSTREAM_NEW=$(CONFIG_FILE="$CONFIG_PATH" KILO_LIST="$KILO_LIST" python3 <<'PYEOF'
import json, os, re
config_path = os.environ["CONFIG_FILE"]
items = json.loads(os.environ["KILO_LIST"])
items = items if isinstance(items, list) else items.get("data") or items.get("models") or []
live_free = {it.get("id") or it.get("name") for it in items if isinstance(it, dict) and it.get("isFree") is True}
with open(config_path, "r", encoding="utf-8") as f:
    text = f.read()
m = re.search(r"^  - name: kilo\s*$(.*?)(?=^  - name:|\Z)", text, re.S | re.M)
block = m.group(1) if m else ""
known = set(re.findall(r"^\s+- id: (\S+)", block, re.M))
print(",".join(sorted(live_free - known)) if live_free - known else "(none)")
PYEOF
) || UPSTREAM_NEW="(unknown)"

# ── OpenCode 匿名车道探测（只报告）──────────────────────────────────────────
# 复刻官方客户端头；项目 id 每次新铸（匿名配额按项目 id 分桶），会话用一次性随机值。
OPENCODE_STATUS="unreachable"
if [ -n "$OPENCODE_LIST" ]; then
  if printf '%s' "$OPENCODE_LIST" | grep -q "\"$OPENCODE_PROBE_MODEL\""; then OC_LISTED=1; else OC_LISTED=0; fi
  OC_COORDS=$(python3 - <<'PYEOF'
import hashlib, secrets
project = hashlib.sha256(b"opencode-project\0" + secrets.token_hex(16).encode()).hexdigest()[:40]
sess = "ses_" + secrets.token_hex(6) + secrets.token_hex(7)
print(project)
print(sess)
PYEOF
)
  OC_PROJ=$(printf '%s\n' "$OC_COORDS" | sed -n '1p')
  OC_SESS=$(printf '%s\n' "$OC_COORDS" | sed -n '2p')
  OC_CODE=$(curl -s -o /tmp/sync-free-oc.$$ -w '%{http_code}' --max-time 90 -X POST \
    "https://opencode.ai/inference/v1/chat/completions" \
    -H "Authorization: Bearer public" -H "Content-Type: application/json" \
    -H "User-Agent: opencode/latest/2.1.1/cli" -H "x-opencode-client: cli" \
    -H "x-session-affinity: $OC_SESS" -H "x-session-id: $OC_SESS" \
    -H "x-opencode-session: $OC_SESS" -H "x-opencode-session-id: $OC_SESS" \
    -H "x-opencode-project: $OC_PROJ" \
    -d "{\"model\":\"$OPENCODE_PROBE_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_completion_tokens\":8}") || OC_CODE="000"
  OPENCODE_STATUS=$(OC_CODE="${OC_CODE:-000}" OC_BODY_FILE="/tmp/sync-free-oc.$$" OC_LISTED="$OC_LISTED" python3 <<'PYEOF'
import json, os
code = os.environ.get("OC_CODE", "000")
listed = os.environ.get("OC_LISTED") == "1"
note = "模型在实时清单中" if listed else "模型不在实时清单中"
try:
    with open(os.environ["OC_BODY_FILE"], "r", encoding="utf-8") as f:
        raw = f.read()
except Exception:
    raw = ""
err_type = ""
if raw:
    try:
        err_type = json.loads(raw).get("error", {}).get("type", "")
    except Exception:
        pass
if code == "200":
    print(f"ok（{note}）")
elif err_type == "FreeUsageLimitError":
    print(f"quota_exhausted（{note}；本机出口 IP 匿名免费额度已用尽）")
elif code == "429":
    print(f"rate_limited（{note}）")
elif code == "000":
    print("unreachable（请求失败）")
else:
    print(f"http_{code}（{note}；err={err_type or 'unknown'}）")
PYEOF
) || OPENCODE_STATUS="error"
  rm -f /tmp/sync-free-oc.$$
fi

# dsh registry 上下文（只报告 free-tier 家族数量）
DSH_NOTE="unavailable"
if [ -n "${DSH_REGISTRY:-}" ]; then
  DSH_NOTE=$(printf '%s' "$DSH_REGISTRY" | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    free = [p.get('id') for p in d if p.get('category') == 'free-tier']
    print(f\"registry 可用: {len(d)} 家提供商，其中 free-tier {len(free)} 家\")
except Exception:
    print('registry 解析失败')
")
fi

# ── 执行修剪 ─────────────────────────────────────────────────────────────────
RELOAD_NOTE="skipped"
PRUNED_NOTE="(none)"
if [ "$PRUNE_IDS" != "(none)" ] && [ -n "$RANGES" ]; then
  if [ "$REPORT_ONLY" -eq 1 ]; then
    RELOAD_NOTE="report-only"
  elif [ "$DRY_RUN" -eq 1 ]; then
    RELOAD_NOTE="dry-run"
  else
    ADMIN_TOKEN=$(find_admin_token) || die "找不到可用的管理凭据（设 EASYGATEWAY_MASTER_KEY 或检查网关地址）"
    GEN_BEFORE=$(admin_get /admin/config/status | python3 -c "import json,sys; print(json.load(sys.stdin).get('generation',''))" || echo "?")
    TS=$(date +%Y%m%d-%H%M%S)
    BACKUP="${CONFIG_PATH}.sync-bak-${TS}"
    cp "$CONFIG_PATH" "$BACKUP"
    # 文本级删除：条目行区间 + 紧邻其上的注释行
    CONFIG_FILE="$CONFIG_PATH" RANGES="$RANGES" python3 <<'PYEOF'
import os
path = os.environ["CONFIG_FILE"]
ranges = []
for item in filter(None, os.environ["RANGES"].split(";")):
    spec, name = item.split("=", 1)  # 模型 id 可能含 ":"（如 xxx:free），用 "=" 分隔
    a, b = (int(x) for x in spec.split("-"))
    ranges.append((a, b, name))
with open(path, "r", encoding="utf-8") as f:
    lines = f.readlines()
drop = set()
for a, b, _name in ranges:
    drop.update(range(a, b + 1))
    # 条目上方紧邻的同级注释（6 空格起）一并移除；不越过 provider 级注释（2 空格）
    j = a - 1
    while j >= 0 and lines[j].lstrip().startswith("#") and len(lines[j]) - len(lines[j].lstrip(" ")) >= 6:
        drop.add(j)
        j -= 1
with open(path, "w", encoding="utf-8") as f:
    f.writelines(line for i, line in enumerate(lines) if i not in drop)
PYEOF
    # 网关侧离线预检（读取磁盘上的新文件）
    if ! admin_post /admin/config/check >/dev/null 2>&1; then
      cp "$BACKUP" "$CONFIG_PATH"
      die "修改后的配置未通过网关预检，已还原备份: $BACKUP"
    fi
    if ! admin_post /admin/config/reload >/dev/null 2>&1; then
      cp "$BACKUP" "$CONFIG_PATH"
      admin_post /admin/config/reload >/dev/null 2>&1 || true
      die "热重载失败，已还原备份并重载旧配置: $BACKUP"
    fi
    GEN_AFTER=$(admin_get /admin/config/status | python3 -c "import json,sys; print(json.load(sys.stdin).get('generation',''))" || echo "?")
    if [ "$GEN_BEFORE" != "?" ] && [ "$GEN_AFTER" != "?" ] && [ "$GEN_AFTER" -le "$GEN_BEFORE" ]; then
      log "警告: 配置代次未递增（$GEN_BEFORE -> $GEN_AFTER），请人工检查 /admin/config/status"
    fi
    RELOAD_NOTE="ok (generation $GEN_BEFORE -> $GEN_AFTER, backup: $(basename "$BACKUP"))"
    # 备份轮换：只保留最近 BACKUP_KEEP 份
    ls -t "${CONFIG_PATH}".sync-bak-* 2>/dev/null | tail -n +$((BACKUP_KEEP + 1)) | while IFS= read -r old; do
      rm -f "$old"
    done
  fi
  PRUNED_NOTE="$PRUNE_IDS"
fi
if [ -n "$CHAIN_BLOCKED" ]; then
  log "注意: 以下模型已失效但被 chains 引用，未自动修剪，需人工处理: $CHAIN_BLOCKED"
fi

# ── 主力模型探活（经网关真实转发，各一次小请求）───────────────────────────────
probe_model() {
  local model="$1" token="$2" http_code body
  http_code=$(curl -s -o /tmp/sync-free-probe.$$ -w '%{http_code}' --max-time 90 -X POST \
    -H "Authorization: Bearer $token" -H "Content-Type: application/json" \
    -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":16}" \
    "$ENDPOINT/v1/chat/completions") || { echo "error"; return; }
  body=$(head -c 400 /tmp/sync-free-probe.$$ 2>/dev/null || true)
  rm -f /tmp/sync-free-probe.$$
  if [ "$http_code" = "200" ]; then echo "ok"; else echo "http_$http_code"; fi
}

ADMIN_TOKEN=$(find_admin_token 2>/dev/null || true)
PROBE_RESULTS=()
if [ -n "$ADMIN_TOKEN" ]; then
  for m in "${PROBE_MODELS[@]}"; do
    PROBE_RESULTS+=("$m=$(probe_model "$m" "$ADMIN_TOKEN")")
  done
  DOCTOR=$(admin_post /admin/models/doctor '{"probe_anonymous":true}' 2>/dev/null | \
    python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    models = d.get('models') or {}
    bad = [f'{name}:{info.get(\"status\")}' for name, info in models.items()
           if isinstance(info, dict) and info.get('status') not in ('available', 'unknown')]
    print(f\"匿名探活 {d.get('probed', '?')} 个模型，异常: {', '.join(bad) if bad else '无'}\")
except Exception as exc:
    print(f'doctor 解析失败: {exc}')
") || DOCTOR="doctor 调用失败"
else
  PROBE_RESULTS+=("网关管理凭据不可用，跳过探活")
  DOCTOR="skipped"
fi

# ── 汇总报告 ─────────────────────────────────────────────────────────────────
log "-- 上游比对 --"
log "kilo 修剪: $PRUNED_NOTE ${RELOAD_NOTE:+($RELOAD_NOTE)}"
log "kilo 上游新增(未自动加入): $UPSTREAM_NEW"
log "OpenCode 公共车道($OPENCODE_PROBE_MODEL): $OPENCODE_STATUS"
log "dsh registry: $DSH_NOTE"
log "-- 主力探活 --"
for r in "${PROBE_RESULTS[@]}"; do log "$r"; done
log "匿名模型 doctor: $DOCTOR"
log "== 结束 =="

mkdir -p "$(dirname "$LOG_PATH")"
{
  printf '%s | pruned=%s reload=%s | opencode=%s | %s\n' \
    "$(date '+%Y-%m-%d %H:%M:%S')" "$PRUNED_NOTE" "$RELOAD_NOTE" "$OPENCODE_STATUS" \
    "$(IFS='; '; echo "${PROBE_RESULTS[*]}")"
} >> "$LOG_PATH"
