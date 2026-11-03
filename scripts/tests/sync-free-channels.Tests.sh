#!/usr/bin/env bash
# sync-free-channels.sh 的离线测试：stub curl 按返回 canned 响应，验证
# 修剪落盘、dry-run、chains 引用保护与上游清单不可用时的守卫。
set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
STUB_DIR=$(mktemp -d)
trap 'rm -rf "$STUB_DIR"' EXIT

FIXTURE_LIVE_KILO='{"data":[{"id":"kilo-auto/free","isFree":true},{"id":"nvidia/nemotron-3-ultra-550b-a55b:free","isFree":true},{"id":"paid/model","isFree":false}]}'
FIXTURE_OPENCODE='{"data":[{"id":"deepseek-v4.1-flash-free"},{"id":"glm-5.3-flash-free"},{"id":"gpt-6.1-sol"}]}'
FIXTURE_REGISTRY='[{"id":"kilo","category":"free-tier"},{"id":"opencode_zen","category":"free-tier"},{"id":"glm","category":"apikey"}]'
FIXTURE_CONFIG='# 模型提供商配置
cors:
  allowed_origins: []

providers:
  - name: glm
    type: openai
    url: https://open.bigmodel.cn/api/coding/paas/v4/chat/completions
    api_key_env: GLM_API_KEY
    models:
      - id: glm-5.3-flash
        capabilities: [text, tool_calling, streaming, reasoning]
  # Kilo AI 公共网关免费池：无需 Key。上游会记录 prompt；清单由同步脚本维护。
  - name: kilo
    type: openai
    url: https://api.kilo.ai/api/gateway/chat/completions
    api_key_env: KILO_API_KEY
    allow_no_key: true
    models:
      # 自动路由档：上游在免费池内自选
      - id: kilo-auto/free
        capabilities: [text, tool_calling, streaming]
      # 已下线的候选（测试用死条目）
      - id: dead/model:free
        capabilities: [text, tool_calling, streaming]
      # 大参数免费档
      - id: nvidia/nemotron-3-ultra-550b-a55b:free
        capabilities: [text, tool_calling, streaming]
  - name: opencode-systemone-free
    type: systemone
    url: https://opencode.ai/inference/systemone/v1/systemone
    api_key_env: OPENCODE_API_KEY
    allow_no_key: true
    models:
      - id: jev-1.13-free
        protocol: systemone
        capabilities: [systemone]

chains:
  coding:
    - glm-5.3-flash
    - kilo-auto/free
'

make_curl_stub() { # $1: 失败的 URL 片段（可空）
  local fail_pattern="${1:-__none__}"
  cat > "$STUB_DIR/curl" <<STUB
#!/usr/bin/env bash
# 按 URL 分发 canned 响应；\$STUB_LOG 记录每次调用，\$STUB_FAIL 匹配则失败
url=""
for arg in "\$@"; do
  case "\$arg" in
    http*) url="\$arg" ;;
  esac
done
printf '%s\n' "\$url" >> "\$STUB_LOG"
case "\$url" in
  *"$fail_pattern"*) exit 22 ;;
  *"/admin/health") exit 0 ;;
  *"/inference/v1/models") printf '%s' '$FIXTURE_OPENCODE' ;;
  *"/api/gateway/models") printf '%s' '$FIXTURE_LIVE_KILO' ;;
  *"registry.json") printf '%s' '$FIXTURE_REGISTRY' ;;
  *"/admin/config/status")
    if [ -f "\$STUB_LOG.reloaded" ]; then printf '{"generation": 8}'; else printf '{"generation": 7}'; fi ;;
  *"/admin/config/check") printf '{"valid": true}' ;;
  *"/admin/config/reload") touch "\$STUB_LOG.reloaded"; printf '{"generation": 8}' ;;
  *"/admin/models/doctor") printf '{"models": []}' ;;
  *"inference/v1/chat/completions") printf '{"error":{"type":"FreeUsageLimitError"}}' ;;
  *"/v1/chat/completions") printf '{"choices":[{"message":{"content":"pong"}}]}' ;;
  *) exit 22 ;;
esac
STUB
  chmod 0755 "$STUB_DIR/curl"
}

run_sync() { # $1=配置路径 $2=输出捕获文件；其余参数透传
  local config="$1" out="$2"; shift 2
  PATH="$STUB_DIR:$PATH" EASYGATEWAY_MASTER_KEY=test-master-key \
    EASYGATEWAY_ENDPOINT=http://127.0.0.1:4001 EASYGATEWAY_CONFIG="$config" \
    EASYGATEWAY_SYNC_LOG="$STUB_DIR/sync.log" STUB_LOG="$STUB_DIR/curl.log" \
    bash "$ROOT_DIR/scripts/sync-free-channels.sh" "$@" >"$out" 2>&1
}

echo "== 场景 A: 失效模型自动修剪，活模型与注释保留 =="
mkdir -p "$STUB_DIR/a"
printf '%s' "$FIXTURE_CONFIG" > "$STUB_DIR/a/providers.yaml"
: > "$STUB_DIR/curl.log"
make_curl_stub ""
run_sync "$STUB_DIR/a/providers.yaml" "$STUB_DIR/a/out" || {
  echo "脚本不应失败"; cat "$STUB_DIR/a/out" >&2; exit 1; }
if grep -q "dead/model:free" "$STUB_DIR/a/providers.yaml"; then
  echo "死条目应被修剪"; exit 1
fi
grep -q "id: kilo-auto/free" "$STUB_DIR/a/providers.yaml" || { echo "活条目不应被删"; exit 1; }
grep -q "id: nvidia/nemotron-3-ultra-550b-a55b:free" "$STUB_DIR/a/providers.yaml" || { echo "活条目不应被删"; exit 1; }
grep -q "自动路由档" "$STUB_DIR/a/providers.yaml" || { echo "条目注释不应被误删"; exit 1; }
grep -q "id: jev-1.13-free" "$STUB_DIR/a/providers.yaml" || { echo "kilo 之后的 provider 不应受影响"; exit 1; }
grep -q "# Jev\|opencode-systemone-free" "$STUB_DIR/a/providers.yaml" || { echo "后续 provider 注释不应被吞"; exit 1; }
ls "$STUB_DIR/a"/providers.yaml.sync-bak-* >/dev/null 2>&1 || { echo "应生成备份"; exit 1; }
grep -q "/admin/config/check" "$STUB_DIR/curl.log" || { echo "应调用预检"; exit 1; }
grep -q "/admin/config/reload" "$STUB_DIR/curl.log" || { echo "应调用热重载"; exit 1; }
grep -q "dead/model:free" "$STUB_DIR/a/out" || { echo "报告应列出被修剪的模型"; exit 1; }
echo "通过"

echo "== 场景 B: --dry-run 不落盘 =="
mkdir -p "$STUB_DIR/b"
printf '%s' "$FIXTURE_CONFIG" > "$STUB_DIR/b/providers.yaml"
: > "$STUB_DIR/curl.log"
make_curl_stub ""
run_sync "$STUB_DIR/b/providers.yaml" "$STUB_DIR/b/out" --dry-run || {
  echo "dry-run 不应失败"; cat "$STUB_DIR/b/out" >&2; exit 1; }
grep -q "dead/model:free" "$STUB_DIR/b/providers.yaml" || { echo "dry-run 不应修改文件"; exit 1; }
ls "$STUB_DIR/b"/providers.yaml.sync-bak-* >/dev/null 2>&1 && { echo "dry-run 不应生成备份"; exit 1; }
grep -q "/admin/config/reload" "$STUB_DIR/curl.log" && { echo "dry-run 不应触发热重载"; exit 1; }
echo "通过"

echo "== 场景 C: 被 chains 引用的失效模型只报告不修剪 =="
mkdir -p "$STUB_DIR/c"
{ printf '%s' "$FIXTURE_CONFIG" | sed 's/    - kilo-auto\/free/    - kilo-auto\/free\n    - dead\/model:free/'; } > "$STUB_DIR/c/providers.yaml"
: > "$STUB_DIR/curl.log"
make_curl_stub ""
run_sync "$STUB_DIR/c/providers.yaml" "$STUB_DIR/c/out" || {
  echo "chain 冲突不应导致失败"; cat "$STUB_DIR/c/out" >&2; exit 1; }
grep -q "dead/model:free" "$STUB_DIR/c/providers.yaml" || { echo "chains 引用的模型不应被修剪"; exit 1; }
grep -q "需人工处理" "$STUB_DIR/c/out" || { echo "应提示需人工处理"; exit 1; }
echo "通过"

echo "== 场景 D: Kilo 清单不可用时守卫退出，不改文件 =="
mkdir -p "$STUB_DIR/d"
printf '%s' "$FIXTURE_CONFIG" > "$STUB_DIR/d/providers.yaml"
: > "$STUB_DIR/curl.log"
make_curl_stub "/api/gateway/models"
if run_sync "$STUB_DIR/d/providers.yaml" "$STUB_DIR/d/out"; then
  echo "上游清单不可用时应失败退出"; cat "$STUB_DIR/d/out" >&2; exit 1
fi
grep -q "dead/model:free" "$STUB_DIR/d/providers.yaml" || { echo "守卫退出不应修改文件"; exit 1; }
ls "$STUB_DIR/d"/providers.yaml.sync-bak-* >/dev/null 2>&1 && { echo "守卫退出不应生成备份"; exit 1; }
grep -q "Kilo 清单不可用" "$STUB_DIR/d/out" || { echo "应说明跳过原因"; exit 1; }
echo "通过"

echo "sync-free-channels 全部场景通过"
