#!/usr/bin/env bash
#
# EasyGateway .deb 打包脚本
#
# 用法:
#   scripts/build-deb.sh <version> <arch> [output-dir]
#
#   version   deb 版本号（不带 v 前缀，如 0.3.6；脚本自动追加 debian 修订号 -1）
#   arch      目标架构: amd64 | arm64
#   output    输出目录（默认 dist/）
#
# 环境变量:
#   GATEWAY_BIN   跳过 go build，直接使用指定预编译二进制
#   LDFLAGS       覆盖 go build 的 -ldflags（默认注入 main.version=v<version>）
#
# 产物布局（系统级安装）:
#   /usr/bin/gateway                              网关二进制（静态编译）
#   /etc/easygateway/providers.yaml               Provider 配置（conffile，升级保留本地修改）
#   /etc/easygateway/easygateway.env              环境变量（postinst 首次安装时生成，含自动 Master Key）
#   /var/lib/easygateway                          运行数据（SQLite、技能同步等）
#   /lib/systemd/system/easygateway.service       systemd 服务
#
# 包名为 easygateway-server，与桌面客户端 deb（mygo 打包，包名 easygateway）区分。
#
set -euo pipefail

usage() { sed -n '2,20p' "$0" >&2; exit 1; }

[ $# -ge 2 ] || usage
VERSION="$1"
ARCH="$2"
OUT_DIR="${3:-dist}"

case "$ARCH" in
    amd64|arm64) ;;
    *) echo "不支持的架构: $ARCH（仅支持 amd64/arm64）" >&2; exit 1 ;;
esac

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
STAGING="$(mktemp -d /tmp/easygateway-deb.XXXXXX)"
trap 'rm -rf "$STAGING"' EXIT

DEB_VERSION="${VERSION}-1"
PKG="easygateway-server_${DEB_VERSION}_${ARCH}"

# ── 1. 编译二进制 ──
mkdir -p "${STAGING}/usr/bin"
if [ -n "${GATEWAY_BIN:-}" ]; then
    cp "${GATEWAY_BIN}" "${STAGING}/usr/bin/gateway"
else
    LDFLAGS="${LDFLAGS:--s -w -X main.version=v${VERSION}}"
    (cd "${REPO_ROOT}/go-gateway" \
        && CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" \
        go build -trimpath -ldflags "${LDFLAGS}" -o "${STAGING}/usr/bin/gateway" .)
fi
chmod 0755 "${STAGING}/usr/bin/gateway"

# ── 2. 配置文件 ──
mkdir -p "${STAGING}/etc/easygateway"
cp "${REPO_ROOT}/go-gateway/providers.yaml" "${STAGING}/etc/easygateway/providers.yaml"
chmod 0644 "${STAGING}/etc/easygateway/providers.yaml"

# ── 3. systemd 服务 ──
mkdir -p "${STAGING}/lib/systemd/system"
cp "${REPO_ROOT}/deploy/systemd/easygateway.service" "${STAGING}/lib/systemd/system/easygateway.service"
chmod 0644 "${STAGING}/lib/systemd/system/easygateway.service"

# ── 4. 数据目录（属主由 postinst 调整） ──
mkdir -p "${STAGING}/var/lib/easygateway"
chmod 0755 "${STAGING}/var/lib/easygateway"

# ── 5. 控制信息与维护脚本 ──
mkdir -p "${STAGING}/DEBIAN"
# 旧服务端包（v0.3.6-1 及更早叫 easygateway）迁移：Conflicts+Replaces 让
# dpkg -i 直接可用；版本上界取第一个桌面包版本 0.3.7，桌面包自身不受影响。
cat > "${STAGING}/DEBIAN/control" <<EOF
Package: easygateway-server
Version: ${DEB_VERSION}
Section: net
Priority: optional
Architecture: ${ARCH}
Depends: adduser
Conflicts: easygateway (<< 0.3.7)
Replaces: easygateway
Provides: easygateway
Maintainer: weijian <weijian@example.com>
Homepage: https://github.com/nightgopher/litellm-gateway
Description: EasyGateway - personal AI infrastructure gateway (server)
 Unified AI gateway written in Go: one endpoint for Claude Code,
 Codex CLI and other AI apps, with OpenAI Chat Completions,
 Anthropic Messages and OpenAI Responses protocol support,
 capability-based model routing, multi-provider fallback and
 circuit breaking. Ships a systemd service running as the
 dedicated "easygateway" user.
EOF

cat > "${STAGING}/DEBIAN/conffiles" <<'EOF'
/etc/easygateway/providers.yaml
EOF

cat > "${STAGING}/DEBIAN/postinst" <<'EOF'
#!/bin/sh
# 首次安装：创建系统用户、数据目录和带随机 Master Key 的环境文件；
# 随后 reload/enable/start（升级路径为 try-restart）。
set -e

PKG_USER=easygateway
PKG_GROUP=easygateway
DATA_DIR=/var/lib/easygateway
ENV_FILE=/etc/easygateway/easygateway.env
SERVICE=easygateway.service

case "$1" in
    configure)
        if ! getent group "$PKG_GROUP" >/dev/null 2>&1; then
            addgroup --system --quiet "$PKG_GROUP"
        fi
        if ! getent passwd "$PKG_USER" >/dev/null 2>&1; then
            adduser --system --ingroup "$PKG_GROUP" --home "$DATA_DIR" \
                --no-create-home --shell /usr/sbin/nologin --quiet "$PKG_USER"
        fi
        install -d -o "$PKG_USER" -g "$PKG_GROUP" -m 0700 "$DATA_DIR"

        if [ ! -f "$ENV_FILE" ]; then
            MASTER_KEY="sk-$(head -c 24 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 32)"
            umask 027
            cat > "$ENV_FILE" <<EOT
# EasyGateway 环境变量（首次安装自动生成）
# 网关认证 token，客户端（如 Claude Code）连接时使用
LITELLM_MASTER_KEY=${MASTER_KEY}

# 提供商 API Key（按需追加，参见 providers.yaml）
# GLM_API_KEY=

# 网关端口
PORT=4001

# 日志级别
LOG_LEVEL=info

# 运行数据目录（SQLite、技能同步等）
PI_GO_DATA_DIR=/var/lib/easygateway
LLM_GATEWAY_HOME=/var/lib/easygateway
EOT
            chown root:"$PKG_GROUP" "$ENV_FILE"
            chmod 0640 "$ENV_FILE"
            echo "easygateway: 已生成配置 $ENV_FILE（含随机 Master Key）"
        fi

        if [ -d /run/systemd/system ]; then
            systemctl --system daemon-reload >/dev/null || true
            deb-systemd-helper enable "$SERVICE" >/dev/null 2>&1 || true
            if [ -n "$2" ]; then
                deb-systemd-invoke try-restart "$SERVICE" >/dev/null || true
            else
                deb-systemd-invoke start "$SERVICE" >/dev/null || true
            fi
        fi
        ;;
esac
EOF

cat > "${STAGING}/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e

if [ -d /run/systemd/system ] && [ "$1" = remove ]; then
    deb-systemd-invoke stop easygateway.service >/dev/null || true
fi
EOF

cat > "${STAGING}/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e

if [ "$1" = purge ]; then
    deb-systemd-helper purge easygateway.service >/dev/null 2>&1 || true
    deb-systemd-helper unmask easygateway.service >/dev/null 2>&1 || true
    rm -f /etc/easygateway/easygateway.env
    rmdir --ignore-fail-on-non-empty /etc/easygateway 2>/dev/null || true
    rm -rf /var/lib/easygateway
    if getent passwd easygateway >/dev/null 2>&1; then
        userdel easygateway >/dev/null 2>&1 || true
    fi
    if getent group easygateway >/dev/null 2>&1; then
        groupdel easygateway >/dev/null 2>&1 || true
    fi
fi
EOF

chmod 0755 "${STAGING}/DEBIAN/postinst" "${STAGING}/DEBIAN/prerm" "${STAGING}/DEBIAN/postrm"

# ── 6. 文档 ──
DOC_DIR="${STAGING}/usr/share/doc/easygateway"
mkdir -p "${DOC_DIR}"
cp "${REPO_ROOT}/README.md" "${DOC_DIR}/README.md"
gzip -9n -c "${REPO_ROOT}/go-gateway/README.md" > "${DOC_DIR}/gateway-README.md.gz"
cat > "${DOC_DIR}/copyright" <<EOF
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: easygateway-server
Source: https://github.com/nightgopher/litellm-gateway

Files: *
Copyright: 2026 weijian <weijian@example.com>
License: MIT
EOF
gzip -9n > "${DOC_DIR}/changelog.Debian.gz" <<EOF
easygateway-server (${DEB_VERSION}) unstable; urgency=medium

  * Release ${VERSION}.

 -- weijian <weijian@example.com>  $(date -R)
EOF

# ── 7. 规范化权限（避免宿主 umask 影响包内权限） ──
find "${STAGING}" -type d -exec chmod 0755 {} +
find "${DOC_DIR}" -type f -exec chmod 0644 {} +

# ── 8. 构建 .deb ──
mkdir -p "${OUT_DIR}"
DEB_PATH="${OUT_DIR}/${PKG}.deb"
rm -f "${DEB_PATH}"
dpkg-deb --root-owner-group --build "${STAGING}" "${DEB_PATH}"
echo "构建完成: ${DEB_PATH}"
