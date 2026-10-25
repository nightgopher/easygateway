#!/usr/bin/env bash
#
# llm-gateway 一键安装脚本
#
# 用法:
#   curl -fsSL https://raw.githubusercontent.com/nightgopher/easygateway/main/scripts/install.sh | bash
#
# 脚本会自动完成:
#   1. 检测系统 → 下载预编译二进制（或编译源码）
#   2. 安装到 ~/.llm-gateway/bin 并配置 PATH
#   3. 下载 providers.yaml 配置
#   4. 引导用户配置 API Key
#   5. 创建 llm-gateway 全局命令
#
set -euo pipefail

REPO="nightgopher/easygateway"
INSTALL_ROOT="${LLM_GATEWAY_HOME:-$HOME/.llm-gateway}"
INSTALL_DIR="${INSTALL_ROOT}/bin"
BINARY_NAME="gateway"
CONFIG_FILE="${INSTALL_ROOT}/.env"
PROVIDERS_FILE="${INSTALL_ROOT}/providers.yaml"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

info()  { echo -e "${CYAN}ℹ${NC}  $*"; }
ok()    { echo -e "${GREEN}✓${NC}  $*"; }
warn()  { echo -e "${YELLOW}⚠${NC}  $*"; }
fail()  { echo -e "${RED}✗${NC}  $*"; exit 1; }

# ── Banner ──
echo ""
echo -e "${BOLD}  ╔══════════════════════════════════════╗${NC}"
echo -e "${BOLD}  ║     EasyGateway Installer           ║${NC}"
echo -e "${BOLD}  ║   轻量级 LLM API 网关 (18MB 内存)   ║${NC}"
echo -e "${BOLD}  ╚══════════════════════════════════════╝${NC}"
echo ""

# ── 1. 检测平台 ──
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$OS" in
    linux)  OS="linux"  ;;
    darwin) OS="darwin" ;;
    *)      fail "不支持的系统: $OS（仅支持 Linux/macOS，Windows 请使用 WSL）" ;;
esac
case "$ARCH" in
    x86_64|amd64)  ARCH="amd64"  ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)             fail "不支持的架构: $ARCH" ;;
esac
info "平台: ${OS}/${ARCH}"

# ── 2. 安装二进制 ──
mkdir -p "${INSTALL_DIR}"

BINARY_PATH="${INSTALL_DIR}/${BINARY_NAME}"
NEEDS_DOWNLOAD=true

# 如果已有相同版本，跳过
if [ -f "${BINARY_PATH}" ]; then
    info "发现已安装的 gateway"
    read -rp "$(echo -e ${CYAN}ℹ${NC}  重新安装/升级? [Y/n] )" REPLY < /dev/tty 2>/dev/null || REPLY="y"
    # macOS ships Bash 3.2, which does not support ${var,,}.
    if [[ "$REPLY" == [nN] ]]; then
        ok "保留现有安装"
        NEEDS_DOWNLOAD=false
    fi
fi

if [ "$NEEDS_DOWNLOAD" = true ]; then
    DOWNLOADED=false
    BINARY_ARTIFACT="gateway-${OS}-${ARCH}"
    URL="https://github.com/${REPO}/releases/latest/download/${BINARY_ARTIFACT}"
    info "下载最新 gateway..."
    if curl -fSL "${URL}" -o "${BINARY_PATH}.tmp" 2>/dev/null; then
        mv "${BINARY_PATH}.tmp" "${BINARY_PATH}"
        chmod +x "${BINARY_PATH}"
        DOWNLOADED=true
        ok "下载完成"
    fi

    if [ "$DOWNLOADED" = false ]; then
        # 回退：从源码编译
        if command -v go &>/dev/null; then
            info "从源码编译（检测到 Go 环境）..."
            TMPDIR=$(mktemp -d)
            git clone --depth 1 "https://github.com/${REPO}.git" "${TMPDIR}/llm-gateway" 2>/dev/null
            cd "${TMPDIR}/llm-gateway/go-gateway"
            CGO_ENABLED=0 go build -ldflags "-X main.version=dev" \
                -o "${BINARY_PATH}" .
            cd - >/dev/null
            rm -rf "${TMPDIR}"
            ok "编译完成"
        else
            fail "无法下载二进制且未安装 Go。\n请安装 Go: https://go.dev/dl/ 后重新运行此脚本。"
        fi
    fi
fi

chmod +x "${BINARY_PATH}" 2>/dev/null || true
ok "二进制: ${BINARY_PATH}"

# ── 3. 配置 PATH ──
PATH_CONFIGURED=false
case ":$PATH:" in
    *":${INSTALL_DIR}:"*)
        PATH_CONFIGURED=true
        ;;
esac

if [ "$PATH_CONFIGURED" = false ]; then
    SHELL_NAME=$(basename "$SHELL")
    case "$SHELL_NAME" in
        zsh)  RC_FILE="$HOME/.zshrc" ;;
        bash) RC_FILE="$HOME/.bashrc" ;;
        fish) RC_FILE="$HOME/.config/fish/config.fish" ;;
        *)    RC_FILE="$HOME/.profile" ;;
    esac

    touch "$RC_FILE"
    if ! grep -q "${INSTALL_DIR}" "$RC_FILE" 2>/dev/null; then
        echo "" >> "$RC_FILE"
        echo "# llm-gateway" >> "$RC_FILE"
        if [ "$SHELL_NAME" = "fish" ]; then
            echo "set -gx PATH \$PATH ${INSTALL_DIR}" >> "$RC_FILE"
        else
            echo "export PATH=\"\$PATH:${INSTALL_DIR}\"" >> "$RC_FILE"
        fi
        ok "PATH 已配置到 ${RC_FILE}"
    fi

    export PATH="$PATH:${INSTALL_DIR}"
fi

# ── 4. 创建 wrapper 脚本 ──
WRAPPER="${INSTALL_DIR}/llm-gateway"
cat > "${WRAPPER}" << 'WRAPEOF'
#!/usr/bin/env bash
# llm-gateway wrapper — 自动加载配置
export GATEWAY_HOME="${LLM_GATEWAY_HOME:-$HOME/.llm-gateway}"
cd "$GATEWAY_HOME"
exec "$GATEWAY_HOME/bin/gateway" "$@"
WRAPEOF
chmod +x "${WRAPPER}"
ok "Wrapper: ${WRAPPER}"

# ── 5. 下载 providers.yaml ──
if [ ! -f "${PROVIDERS_FILE}" ]; then
    info "下载 providers.yaml..."
    if curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/go-gateway/providers.yaml" \
        -o "${PROVIDERS_FILE}" 2>/dev/null; then
        ok "providers.yaml 已下载"
    else
        warn "providers.yaml 下载失败，稍后可手动复制"
    fi
else
    ok "providers.yaml 已存在"
fi

# ── 6. 配置文件 ──
if [ ! -f "${CONFIG_FILE}" ]; then
    info "创建配置: ${CONFIG_FILE}"

    # 生成随机 master key
    MASTER_KEY="sk-$(head -c 24 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 32)"

    echo ""
    echo -e "${BOLD}  ── 配置 EasyGateway ──${NC}"
    echo ""
    echo -e "  选择要启用的提供商:"
    echo -e "  ${CYAN}1${NC}) 智谱 GLM（glm-5.3 / glm-5.3-flash）"
    echo -e "  ${CYAN}2${NC}) 跳过，稍后配置"
    echo ""
    read -rp "$(echo -e ${CYAN}ℹ${NC}  选择 [1 / 2]: )" CHOICES < /dev/tty 2>/dev/null || CHOICES="2"

    GLM_KEY=""

    if [[ "$CHOICES" != *"2"* ]]; then
        for choice in $CHOICES; do
            case "$choice" in
                1)
                    read -rp "$(echo -e ${CYAN}ℹ${NC}  智谱 GLM API Key: )" GLM_KEY < /dev/tty 2>/dev/null || GLM_KEY=""
                    ;;
            esac
        done
    fi

    cat > "${CONFIG_FILE}" << EOF
# EasyGateway 配置文件
# 网关认证 token（自动生成，用于 Claude Code 等客户端连接）
LITELLM_MASTER_KEY=${MASTER_KEY}

# 提供商 API Key（按需填写）
GLM_API_KEY=${GLM_KEY}


# 网关端口
PORT=4001

# 日志级别
LOG_LEVEL=info
EOF
    chmod 600 "${CONFIG_FILE}"
    ok "配置已保存（Master Key 已自动生成）"
else
    ok "配置已存在: ${CONFIG_FILE}"
fi

# ── 7. 完成 ──
echo ""
echo -e "${GREEN}${BOLD}  🎉 安装完成！${NC}"
echo ""
echo -e "  ${BOLD}启动网关:${NC}"
echo -e "     ${GREEN}llm-gateway${NC}"
echo ""
echo -e "  ${BOLD}配置 Claude Code:${NC}"
echo -e "     编辑 ~/.claude/settings.json："
echo -e "     ${CYAN}{"
echo -e "       \"env\": {"
echo -e "         \"ANTHROPIC_BASE_URL\": \"http://localhost:4001/v1\","
echo -e "         \"ANTHROPIC_AUTH_TOKEN\": \"你的 LITELLM_MASTER_KEY\""
echo -e "       }"
echo -e "     }${NC}"
echo ""
echo -e "  ${BOLD}修改配置:${NC}"
echo -e "     ${CYAN}nano ${CONFIG_FILE}${NC}"
echo ""
echo -e "  ${BOLD}修改提供商:${NC}"
echo -e "     ${CYAN}nano ${PROVIDERS_FILE}${NC}"
echo ""
echo -e "  安装路径: ${CYAN}${INSTALL_DIR}${NC}"
echo -e "  配置文件: ${CYAN}${CONFIG_FILE}${NC}"
echo ""
