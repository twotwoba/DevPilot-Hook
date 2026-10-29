// Package config 管理 daemon 的状态目录与配置文件。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/id"
)

// 默认值。
const (
	DefaultGatewayPort            = 24543
	DefaultApprovalTimeoutSeconds = wire.ApprovalWaitSeconds
	DefaultToolEventInterval      = 300
	DefaultApprovalMode           = string(ApprovalModePermissionRequest)

	// DefaultRelayURL 是官方托管中继;pair 未显式指定时使用,
	// 用户零配置。自部署仍可用 --relay 覆盖(双轨)。
	DefaultRelayURL = "https://relay.devpilot.app"
)

// ApprovalMode 决定审批走哪个 hook 事件。
type ApprovalMode string

const (
	// ApprovalModePermissionRequest 用 PermissionRequest 做主审批通道。
	ApprovalModePermissionRequest ApprovalMode = "permission-request"
	// ApprovalModePreToolUse 是旧版 Claude Code 的回退模式。
	ApprovalModePreToolUse ApprovalMode = "pre-tool-use"
)

// Valid 判断审批模式是否合法。
func (m ApprovalMode) Valid() bool {
	return m == ApprovalModePermissionRequest || m == ApprovalModePreToolUse
}

// Paths 是状态目录下的固定文件布局。
type Paths struct {
	Dir string
}

// DefaultPaths 返回用户配置目录下的默认布局。
func DefaultPaths() (Paths, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, fmt.Errorf("config: cannot locate user config directory: %w", err)
	}
	return Paths{Dir: filepath.Join(base, "devpilot-hook")}, nil
}

// NewPaths 用指定目录构造布局（测试用）。
func NewPaths(dir string) Paths { return Paths{Dir: dir} }

// ConfigPath 是主配置文件路径。
func (p Paths) ConfigPath() string { return filepath.Join(p.Dir, "config.json") }

// TokenPath 是本地网关 token 路径。
func (p Paths) TokenPath() string { return filepath.Join(p.Dir, "gateway.token") }

// WALPath 是离线队列路径。
func (p Paths) WALPath() string { return filepath.Join(p.Dir, "queue.wal") }

// Config 是 daemon 的可持久化配置。
type Config struct {
	DaemonID          string   `json:"daemon_id"`
	DaemonCredential  string   `json:"daemon_credential,omitempty"`
	RelayURL          string   `json:"relay_url,omitempty"`
	GatewayPort       int      `json:"gateway_port"`
	ApprovalTimeout   int      `json:"approval_timeout_seconds"`
	ToolEventInterval int      `json:"tool_event_interval_seconds"`
	ApprovalMode      string   `json:"approval_mode"`
	Agents            []string `json:"agents"`
	CreatedAt         int64    `json:"created_at"`
}

// Default 返回一份可直接使用的默认配置。
func Default() *Config {
	return &Config{
		GatewayPort:       DefaultGatewayPort,
		ApprovalTimeout:   DefaultApprovalTimeoutSeconds,
		ToolEventInterval: DefaultToolEventInterval,
		ApprovalMode:      DefaultApprovalMode,
		Agents:            []string{"claude", "codex"},
		CreatedAt:         time.Now().Unix(),
	}
}

// Load 读取配置；文件不存在时返回默认配置（不落盘）。
func Load(p Paths) (*Config, error) {
	data, err := os.ReadFile(p.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: read failed: %w", err)
	}
	cfg := Default()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse failed (refusing to continue to avoid overwriting a corrupted config): %w", err)
	}
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Normalize 补齐缺失字段并校验。
func (c *Config) Normalize() error {
	d := Default()
	if c.GatewayPort == 0 {
		c.GatewayPort = d.GatewayPort
	}
	if c.ApprovalTimeout <= 0 {
		c.ApprovalTimeout = d.ApprovalTimeout
	}
	if c.ToolEventInterval <= 0 {
		c.ToolEventInterval = d.ToolEventInterval
	}
	if c.ApprovalMode == "" {
		c.ApprovalMode = d.ApprovalMode
	}
	if !ApprovalMode(c.ApprovalMode).Valid() {
		return fmt.Errorf("config: unknown approval mode %q", c.ApprovalMode)
	}
	if len(c.Agents) == 0 {
		c.Agents = d.Agents
	}
	if c.CreatedAt == 0 {
		c.CreatedAt = time.Now().Unix()
	}
	// 端口必须落在一个安全范围内；0 表示随机端口，由 serve 写回。
	if c.GatewayPort < 0 || c.GatewayPort > 65535 {
		return fmt.Errorf("config: port out of range %d", c.GatewayPort)
	}
	return nil
}

// EnsureIdentity 在缺失时生成 daemon_id，并返回是否发生了变更。
func (c *Config) EnsureIdentity() bool {
	if c.DaemonID != "" {
		return false
	}
	c.DaemonID = "dd_" + id.RandomHex(16)
	return true
}

// Save 原子写入配置文件（0600）。
func (c *Config) Save(p Paths) error {
	if err := c.Normalize(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteAtomic(p.ConfigPath(), data, 0o600)
}

// LoadOrCreateToken 读取本地网关 token；不存在则生成并以 0600 落盘。
func LoadOrCreateToken(p Paths) (string, error) {
	data, err := os.ReadFile(p.TokenPath())
	if err == nil {
		token := string(data)
		if len(token) < 32 {
			return "", fmt.Errorf("config: local token too short, likely corrupted")
		}
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("config: failed to read local token: %w", err)
	}
	token := id.RandomHex(32)
	if err := WriteAtomic(p.TokenPath(), []byte(token), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// WriteAtomic 先写同目录临时文件并 fsync，再 rename 覆盖目标。
//
// macOS/Linux 的 rename(2) 在同一文件系统内是原子替换；
// 临时文件必须与目标同目录，避免跨文件系统导致 EXDEV。
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: failed to create directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("config: failed to create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("config: failed to set permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("config: failed to write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("config: fsync failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("config: failed to close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("config: atomic replace failed: %w", err)
	}
	return nil
}
