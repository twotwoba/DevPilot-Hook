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
	command -v "$1" >/dev/null 2>&1 || fail "missing dependency: $1"
}
need curl
need tar
need uname

# ---------- 平台识别 ----------
case "$(uname -s)" in
Darwin) os="darwin" ;;
Linux) os="linux" ;;
*) fail "unsupported OS: $(uname -s) (only macOS and Linux are supported)" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch="amd64" ;;
arm64 | aarch64) arch="arm64" ;;
*) fail "unsupported architecture: $(uname -m)" ;;
esac

# ---------- 版本解析 ----------
if [ -n "${DEVPILOT_HOOK_VERSION:-}" ]; then
	tag="$DEVPILOT_HOOK_VERSION"
	case "$tag" in v*) ;; *) tag="v$tag" ;; esac
else
	info "checking latest version"
	tag=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$tag" ] || fail "failed to resolve latest version, set DEVPILOT_HOOK_VERSION to pin one"
fi
version="${tag#v}"

asset="${BIN}_${version}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${tag}"

# ---------- 下载与校验 ----------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

info "downloading ${asset} (${tag})"
curl -fsSL "${base}/${asset}" -o "${tmp}/${asset}" || fail "download failed: ${base}/${asset}"
curl -fsSL "${base}/checksums.txt" -o "${tmp}/checksums.txt" || fail "failed to download checksums.txt"

expected=$(awk -v a="$asset" '$2 == a { print $1 }' "${tmp}/checksums.txt")
[ -n "$expected" ] || fail "${asset} not found in checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "${tmp}/${asset}" | awk '{ print $1 }')
else
	actual=$(shasum -a 256 "${tmp}/${asset}" | awk '{ print $1 }')
fi
[ "$expected" = "$actual" ] || fail "checksum mismatch: expected ${expected}, got ${actual}"

# ---------- 安装 ----------
tar -xzf "${tmp}/${asset}" -C "$tmp" "$BIN" || fail "extraction failed"
bin_dir="${DEVPILOT_HOOK_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$bin_dir"
if [ -w "$bin_dir" ]; then
	install -m 0755 "${tmp}/${BIN}" "${bin_dir}/${BIN}"
else
	info "${bin_dir} is not writable, falling back to sudo"
	sudo install -m 0755 "${tmp}/${BIN}" "${bin_dir}/${BIN}"
fi

info "installed to ${bin_dir}/${BIN}"
"${bin_dir}/${BIN}" version || true

case ":${PATH}:" in
*":${bin_dir}:"*) ;;
*) info "note: add ${bin_dir} to your PATH" ;;
esac

cat <<EOF

Next steps:
  ${BIN} install            # write hooks config for Claude Code / Codex
  ${BIN} service install    # set up auto-start service (launchd / systemd user)
  ${BIN} pair --relay https://relay.example.com
EOF
