#!/bin/sh
# devpilot-hook 的 Linux / macOS 安装脚本。
#
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/twotwoba/DevPilot-Hook/main/install.sh | sh
#
# 可选环境变量：
#   DEVPILOT_HOOK_VERSION     指定版本（默认取最新 release）
#   DEVPILOT_HOOK_INSTALL_DIR 安装目录（默认 ~/.local/bin）
set -eu

REPO="twotwoba/DevPilot-Hook"
BIN="devpilot-hook"

info() { printf '\033[1;34m==>\033[0m %s\n' "$1"; }
fail() { printf '\033[1;31merror:\033[0m %s\n' "$1" >&2; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || fail "缺少依赖：$1"
}
need curl
need tar
need uname

# ---------- 平台识别 ----------
case "$(uname -s)" in
Darwin) os="darwin" ;;
Linux) os="linux" ;;
*) fail "不支持的系统：$(uname -s)（本项目仅支持 macOS 与 Linux）" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch="amd64" ;;
arm64 | aarch64) arch="arm64" ;;
*) fail "不支持的架构：$(uname -m)" ;;
esac

# ---------- 版本解析 ----------
if [ -n "${DEVPILOT_HOOK_VERSION:-}" ]; then
	tag="$DEVPILOT_HOOK_VERSION"
	case "$tag" in v*) ;; *) tag="v$tag" ;; esac
else
	info "查询最新版本"
	tag=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$tag" ] || fail "无法解析最新版本，可用 DEVPILOT_HOOK_VERSION 指定"
fi
version="${tag#v}"

asset="${BIN}_${version}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${tag}"

# ---------- 下载与校验 ----------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

info "下载 ${asset}（${tag}）"
curl -fsSL "${base}/${asset}" -o "${tmp}/${asset}" || fail "下载失败：${base}/${asset}"
curl -fsSL "${base}/checksums.txt" -o "${tmp}/checksums.txt" || fail "下载 checksums.txt 失败"

expected=$(awk -v a="$asset" '$2 == a { print $1 }' "${tmp}/checksums.txt")
[ -n "$expected" ] || fail "checksums.txt 中没有 ${asset}"

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "${tmp}/${asset}" | awk '{ print $1 }')
else
	actual=$(shasum -a 256 "${tmp}/${asset}" | awk '{ print $1 }')
fi
[ "$expected" = "$actual" ] || fail "校验失败：期望 ${expected}，实际 ${actual}"

# ---------- 安装 ----------
tar -xzf "${tmp}/${asset}" -C "$tmp" "$BIN" || fail "解压失败"
bin_dir="${DEVPILOT_HOOK_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$bin_dir"
if [ -w "$bin_dir" ]; then
	install -m 0755 "${tmp}/${BIN}" "${bin_dir}/${BIN}"
else
	info "${bin_dir} 不可写，尝试 sudo"
	sudo install -m 0755 "${tmp}/${BIN}" "${bin_dir}/${BIN}"
fi

info "已安装到 ${bin_dir}/${BIN}"
"${bin_dir}/${BIN}" version || true

case ":${PATH}:" in
*":${bin_dir}:"*) ;;
*) info "提示：请把 ${bin_dir} 加入 PATH" ;;
esac

cat <<EOF

下一步：
  ${BIN} install            # 写入 Claude Code / Codex 的 hooks 配置
  ${BIN} service install    # 配置开机自启（launchd / systemd user）
  ${BIN} pair --relay https://relay.example.com
EOF
