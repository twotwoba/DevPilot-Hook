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
	"net/url"
	"os"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

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
	var noQR bool
	pathsFlag(fs, &dir)
	fs.StringVar(&relay, "relay", "", "relay 地址（缺省使用官方托管中继）")
	fs.BoolVar(&noQR, "no-qr", false, "不在终端渲染配对二维码")
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
	relay = resolveRelay(relay, cfg.RelayURL)

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

	fmt.Fprintln(stdout, "配对会话已创建（15 分钟内有效），在 DevPilot App 中扫码或输入配对码：")
	fmt.Fprintln(stdout)
	if !noQR {
		renderPairQR(stdout, pairQRPayload(relay, code))
	}
	fmt.Fprintf(stdout, "    %s\n\n", code)
	fmt.Fprintf(stdout, "daemon_id: %s\n", cfg.DaemonID)
	fmt.Fprintf(stdout, "relay:     %s\n", relay)
	return 0
}

// resolveRelay 回退链:--relay > config.json 的 relay_url > 官方托管中继。
func resolveRelay(flagVal, saved string) string {
	switch {
	case strings.TrimSpace(flagVal) != "":
		return strings.TrimRight(strings.TrimSpace(flagVal), "/")
	case strings.TrimSpace(saved) != "":
		return strings.TrimRight(strings.TrimSpace(saved), "/")
	default:
		return config.DefaultRelayURL
	}
}

// pairQRPayload 拼装自包含配对链接,App 扫码即可完成配对
// (自部署 relay 非默认地址也能扫,地址编在 payload 里)。
func pairQRPayload(relayURL, code string) string {
	q := url.Values{}
	q.Set("relay", relayURL)
	q.Set("code", code)
	return "devpilot://pair?v=1&" + q.Encode()
}

// renderPairQR 把 payload 渲染成终端半块字符画;
// payload 由本地拼装,渲染失败静默降级(配对码文本始终在下方)。
func renderPairQR(w io.Writer, payload string) {
	qr, err := qrcode.New(payload, qrcode.Low)
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(strings.TrimRight(qr.ToString(false), "\n"), "\n") {
		fmt.Fprintf(w, "    %s\n", line)
	}
	fmt.Fprintln(w)
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
	// 尽力吊销 relay 侧数据(级联删设备、收件箱、待审批等)。
	// 任何失败都静默忽略:relay 可能未升级出该端点(404),也可能本就离线——
	// 吊销不阻塞本地解绑,凭据本身即刻失效。
	if cfg.DaemonID != "" && cfg.DaemonCredential != "" && cfg.RelayURL != "" {
		if ok := revokeDaemon(cfg.RelayURL, cfg.DaemonCredential); ok {
			fmt.Fprintln(stdout, "relay 侧数据已吊销")
		}
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

// revokeDaemon best-effort 调 DELETE /v1/daemon;仅 2xx 视为成功。
func revokeDaemon(relayURL, credential string) bool {
	req, err := http.NewRequest(http.MethodDelete,
		strings.TrimRight(relayURL, "/")+"/v1/daemon", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode/100 == 2
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
