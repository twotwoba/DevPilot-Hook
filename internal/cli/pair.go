package cli

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/twotwoba/DevPilot-Hook/internal/config"
	"github.com/twotwoba/DevPilot-Hook/internal/id"
)

type pairSessionRequest struct {
	Code           string `json:"code"`
	DaemonID       string `json:"daemon_id"`
	DaemonCredHash string `json:"daemon_cred_hash"`
	DaemonName     string `json:"daemon_name"`
	ExpiresAt      int64  `json:"expires_at"`
}

func runPair(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir, relay string
	pathsFlag(fs, &dir)
	fs.StringVar(&relay, "relay", "", "relay 地址，例如 https://relay.example.com")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	paths, err := resolvePaths(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg, err := config.Load(paths)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg.EnsureIdentity()
	if relay == "" {
		relay = cfg.RelayURL
	}
	relay = strings.TrimRight(strings.TrimSpace(relay), "/")
	if relay == "" {
		fmt.Fprintln(stderr, "缺少 --relay（或先写入 config.json 的 relay_url）")
		return 2
	}

	credential, secret := ensureCredential(cfg)
	code, err := pairingCode()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	expires := time.Now().Add(15 * time.Minute).Unix()
	reqBody := pairSessionRequest{
		Code:           code,
		DaemonID:       cfg.DaemonID,
		DaemonCredHash: hashSecret(secret),
		DaemonName:     hostname(),
		ExpiresAt:      expires,
	}
	payload, _ := json.Marshal(reqBody)

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodPost, relay+"/v1/pairing/sessions", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(stderr, "构造配对请求失败: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	// 已注册的 daemon 轮换凭据时，relay 要求出示旧凭据；首次注册时该头会被忽略。
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "注册配对会话失败: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		fmt.Fprintf(stderr, "relay 拒绝配对会话（HTTP %d）: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}

	cfg.RelayURL = relay
	cfg.DaemonCredential = credential
	if err := cfg.Save(paths); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	fmt.Fprintln(stdout, "配对会话已创建，请在 15 分钟内在 DevPilot App 中输入以下配对码：")
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "    %s\n\n", code)
	fmt.Fprintf(stdout, "daemon_id: %s\n", cfg.DaemonID)
	fmt.Fprintf(stdout, "relay:     %s\n", relay)
	return 0
}

func runUnpair(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("unpair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir string
	pathsFlag(fs, &dir)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	paths, err := resolvePaths(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg, err := config.Load(paths)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// 同时清空 daemon_id：relay 侧凭据轮换必须出示旧凭据，
	// 本地若保留旧 daemon_id 又丢掉凭据，将永远无法再次配对。
	cfg.DaemonID = ""
	cfg.DaemonCredential = ""
	if err := cfg.Save(paths); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "已解除本机与 relay 的配对；下次 pair 会生成新的 daemon_id")
	return 0
}

// ensureCredential 生成或复用 daemon 凭据，返回完整凭据与 secret 部分。
func ensureCredential(cfg *config.Config) (credential, secret string) {
	prefix := "dpd_" + cfg.DaemonID + "_"
	if s, ok := strings.CutPrefix(cfg.DaemonCredential, prefix); ok && len(s) >= 32 {
		return cfg.DaemonCredential, s
	}
	secret = id.RandomHex(32)
	return prefix + secret, secret
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// pairingCode 生成 128 位、无易混字符的配对码。
func pairingCode() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成配对码失败: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	return enc, nil
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "devpilot-daemon"
	}
	return name
}
