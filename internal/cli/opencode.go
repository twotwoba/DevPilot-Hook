package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// openCodeVersionTimeout 是 `opencode --version` 的兜底超时：正常是毫秒级
// 进程启动，超时只为防 CLI 卡死拖住 install/doctor。
const openCodeVersionTimeout = 3 * time.Second

// openCodeVersionPattern 匹配版本号（`0.14.3`、`opencode v2.2.6` 都成）。
var openCodeVersionPattern = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// openCodeLegacyPluginName 是旧实现的文件名（plugins/devpilot-hook.mjs）。
// OpenCode 0.x/2.x 都不认 .mjs，安装/卸载时顺手清掉自家旧文件。
const openCodeLegacyPluginName = "devpilot-hook.mjs"

// openCodeConfigDir 返回 OpenCode 全局配置目录。
//
// OpenCode 官方口径：全局配置/插件目录恒 $XDG_CONFIG_HOME/opencode 或
// ~/.config/opencode（跨平台固定写法，macOS 也不走 ~/Library——与 daemon
// 状态目录的 os.UserConfigDir 语义是两套路径，不能混用）。
func openCodeConfigDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot locate home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "opencode"), nil
}

// openCodePluginPath 返回指定变体的插件文件路径。
func openCodePluginPath(v openCodeVariant) (string, error) {
	dir, err := openCodeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, v.pluginDir(), openCodePluginName), nil
}

// openCodeLegacyPluginPath 返回旧实现的 .mjs 路径（仅用于清理）。
func openCodeLegacyPluginPath() (string, error) {
	dir, err := openCodeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "plugins", openCodeLegacyPluginName), nil
}

// openCodePluginPaths 返回我们可能写过的全部插件路径（v1 / v2 / 旧 .mjs）。
func openCodePluginPaths() []string {
	out := make([]string, 0, 3)
	for _, v := range []openCodeVariant{openCodeV1, openCodeV2} {
		if p, err := openCodePluginPath(v); err == nil {
			out = append(out, p)
		}
	}
	if p, err := openCodeLegacyPluginPath(); err == nil {
		out = append(out, p)
	}
	return out
}

// openCodeExtraPaths 是 PATH 之外的兜底安装路径；提成变量供测试关闭，
// 否则开发机上的真实 opencode 会穿透测试隔离。
var openCodeExtraPaths = func() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".opencode", "bin", "opencode"),
		filepath.Join(home, ".local", "bin", "opencode"),
		"/opt/homebrew/bin/opencode",
		"/usr/local/bin/opencode",
		"/usr/bin/opencode",
	}
}

// findOpenCodeBinary 定位 opencode 可执行文件。
//
// 先查 PATH（用户 shell 里实际会跑的那个），再查常见安装路径——launchd /
// systemd 拉起的 self-heal 进程 PATH 很窄，不能只靠 LookPath。
func findOpenCodeBinary() string {
	if p, err := exec.LookPath("opencode"); err == nil {
		return p
	}
	for _, c := range openCodeExtraPaths() {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// detectOpenCodeVariant 探测本机 OpenCode 的插件代际。
//
// 找不到可执行文件或版本解析失败时 found=false，变体回落 v2（当前稳定版
// 口径）；调用方据此决定是否提示用户。
func detectOpenCodeVariant() (v openCodeVariant, version string, found bool) {
	bin := findOpenCodeBinary()
	if bin == "" {
		return openCodeV2, "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), openCodeVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return openCodeV2, "", false
	}
	version = parseOpenCodeVersion(string(out))
	major, ok := openCodeMajor(version)
	if !ok {
		return openCodeV2, version, false
	}
	if major >= 2 {
		return openCodeV2, version, true
	}
	return openCodeV1, version, true
}

// parseOpenCodeVersion 从版本文本里提取 x.y.z。
func parseOpenCodeVersion(out string) string {
	if m := openCodeVersionPattern.FindString(strings.TrimSpace(out)); m != "" {
		return m
	}
	return ""
}

// openCodeMajor 返回版本号主版本。
func openCodeMajor(version string) (int, bool) {
	m := openCodeVersionPattern.FindStringSubmatch(version)
	if m == nil {
		return 0, false
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return major, true
}

// isManagedOpenCodePlugin 判断文件内容是否出自 devpilot-hook。
func isManagedOpenCodePlugin(data []byte) bool {
	return strings.Contains(string(data), openCodePluginMarker)
}

// installOpenCodePlugin 写入（或升级）OpenCode 插件文件。
//
// 按本机 OpenCode 版本选择 v1 / v2 插件 API 与目录；同时清掉另一代际和
// 旧实现的自家文件，避免 OpenCode 加载到错误格式（2.x 对 v1 插件会打
// failed to load 警告）。只覆盖自家文件：目标已存在且不带标记时拒绝写入。
// 内容不变时返回 changed=false（幂等）。
func installOpenCodePlugin(exe string) (changed bool, v openCodeVariant, path string, err error) {
	v, _, _ = detectOpenCodeVariant()
	path, err = openCodePluginPath(v)
	if err != nil {
		return false, v, "", err
	}
	source := openCodePluginSource(v, exe)
	if original, readErr := os.ReadFile(path); readErr == nil {
		if !isManagedOpenCodePlugin(original) {
			return false, v, path, fmt.Errorf("%s exists and is not a devpilot-hook managed plugin; refusing to overwrite", path)
		}
		if strings.TrimSpace(string(original)) == strings.TrimSpace(source) {
			// 内容已最新；仍清理另一代际的残留（版本升级场景）。
			return false, v, path, removeOtherOpenCodePlugins(path)
		}
	}
	if err := config.WriteAtomic(path, []byte(source), 0o644); err != nil {
		return false, v, path, err
	}
	return true, v, path, removeOtherOpenCodePlugins(path)
}

// removeOtherOpenCodePlugins 删除 keep 之外、带自家标记的插件文件。
// 不带标记的用户文件原样保留。
func removeOtherOpenCodePlugins(keep string) error {
	var problems []error
	for _, p := range openCodePluginPaths() {
		if filepath.Clean(p) == filepath.Clean(keep) {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			problems = append(problems, err)
			continue
		}
		if !isManagedOpenCodePlugin(data) {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

// uninstallOpenCodePlugin 删除全部自家插件文件（v1 / v2 / 旧 .mjs）。
// 不带标记的文件原样保留；文件不存在视为已卸载。
func uninstallOpenCodePlugin() (changed bool, removed []string, err error) {
	var problems []error
	for _, p := range openCodePluginPaths() {
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				problems = append(problems, readErr)
			}
			continue
		}
		if !isManagedOpenCodePlugin(data) {
			continue
		}
		if rmErr := os.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
			problems = append(problems, rmErr)
			continue
		}
		changed = true
		removed = append(removed, p)
	}
	return changed, removed, errors.Join(problems...)
}

// openCodePluginState 汇总 doctor 需要的插件状态。
type openCodePluginState struct {
	ExpectedVariant  openCodeVariant // 本机 opencode 版本对应的代际
	ExpectedPath     string
	DetectedVersion  string
	Detected         bool
	Installed        bool
	InstalledPath    string
	InstalledVariant openCodeVariant
	LegacyPath       string // 旧 .mjs 残留（非空表示需要清理）
}

// inspectOpenCodePlugin 检查插件是否按本机版本正确安装。
func inspectOpenCodePlugin() openCodePluginState {
	var st openCodePluginState
	st.ExpectedVariant, st.DetectedVersion, st.Detected = detectOpenCodeVariant()
	st.ExpectedPath, _ = openCodePluginPath(st.ExpectedVariant)
	for _, v := range []openCodeVariant{openCodeV1, openCodeV2} {
		p, err := openCodePluginPath(v)
		if err != nil {
			continue
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil || !isManagedOpenCodePlugin(data) {
			continue
		}
		st.Installed = true
		st.InstalledPath = p
		st.InstalledVariant = v
		break
	}
	if p, err := openCodeLegacyPluginPath(); err == nil {
		if data, readErr := os.ReadFile(p); readErr == nil && isManagedOpenCodePlugin(data) {
			st.LegacyPath = p
		}
	}
	return st
}

// openCodeVariantNote 给 install 输出补一句版本口径，避免用户分不清
// 装的是哪一代插件。
func openCodeVariantNote(v openCodeVariant) string {
	if v == openCodeV2 {
		return ", OpenCode 2.x+"
	}
	return ", OpenCode 0.x"
}
