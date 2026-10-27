package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// pairRequestCapture 是一次 /v1/pairing/sessions 请求的快照。
type pairRequestCapture struct {
	method string
	path   string
	auth   string
	body   pairSessionRequest
}

func TestPairSendsDaemonIDAndCredential(t *testing.T) {
	secret := strings.Repeat("s", 32)
	cred := "dpd_dd_test_" + secret
	var (
		captured pairRequestCapture
		rawBody  []byte
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		rawBody, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(rawBody, &captured.body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"daemon_id":"dd_test"}`)
	}))
	defer ts.Close()

	paths := config.NewPaths(t.TempDir())
	cfg := config.Default()
	cfg.DaemonID = "dd_test"
	cfg.DaemonCredential = cred
	if err := cfg.Save(paths); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runPair([]string{"--dir", paths.Dir, "--relay", ts.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}

	if captured.method != http.MethodPost || captured.path != "/v1/pairing/sessions" {
		t.Fatalf("请求 = %s %s", captured.method, captured.path)
	}
	if captured.auth != "Bearer "+cred {
		t.Fatalf("Authorization = %q", captured.auth)
	}
	if captured.body.DaemonID != "dd_test" {
		t.Fatalf("daemon_id = %q", captured.body.DaemonID)
	}
	sum := hashSecret(secret)
	sentHash := captured.body.DaemonCredHash
	if sentHash != sum {
		t.Fatalf("凭据哈希 = %q，期望 %q", sentHash, sum)
	}
	// 哈希必须是 SHA-256 hex（64 字符），且不包含明文 secret。
	if len(sentHash) != 64 || strings.Contains(string(rawBody), secret) {
		t.Fatalf("请求体泄露 secret: %s", rawBody)
	}
	if _, err := hex.DecodeString(sentHash); err != nil {
		t.Fatalf("哈希不是 hex: %v", err)
	}

	saved, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RelayURL != ts.URL || saved.DaemonCredential != cred {
		t.Fatalf("配对后配置 = %+v", saved)
	}
	if !strings.Contains(stdout.String(), captured.body.Code) {
		t.Fatal("未输出配对码")
	}

	// 首次注册成功后 relay 可能返回 2xx；错误响应必须非零退出。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer bad.Close()
	stdout.Reset()
	stderr.Reset()
	if code := runPair([]string{"--dir", paths.Dir, "--relay", bad.URL}, &stdout, &stderr); code == 0 {
		t.Fatal("HTTP 401 应导致非零退出")
	}
	if !strings.Contains(stderr.String(), "401") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestUnpairClearsIdentity(t *testing.T) {
	paths := config.NewPaths(t.TempDir())
	cfg := config.Default()
	cfg.DaemonID = "dd_test"
	cfg.DaemonCredential = "dpd_dd_test_" + strings.Repeat("s", 32)
	if err := cfg.Save(paths); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runUnpair([]string{"--dir", paths.Dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	saved, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	// 两者必须同时清空：只清凭据会让 relay 的轮换鉴权永久失败。
	if saved.DaemonID != "" || saved.DaemonCredential != "" {
		t.Fatalf("unpair 后 daemon_id=%q credential=%q", saved.DaemonID, saved.DaemonCredential)
	}
}

func TestResolveRelay(t *testing.T) {
	cases := []struct {
		name  string
		flag  string
		saved string
		want  string
	}{
		{"flag 优先", "https://a.example.com", "https://b.example.com", "https://a.example.com"},
		{"其次落盘配置", "", "https://b.example.com/", "https://b.example.com"},
		{"最后官方中继", "", "", config.DefaultRelayURL},
		{"空白值视为未填", "  ", "  ", config.DefaultRelayURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveRelay(c.flag, c.saved); got != c.want {
				t.Fatalf("resolveRelay(%q,%q) = %q, want %q", c.flag, c.saved, got, c.want)
			}
		})
	}
}

func TestPairQRPayload(t *testing.T) {
	got := pairQRPayload("https://relay.devpilot.app", "ABC234567")
	want := "devpilot://pair?v=1&code=ABC234567&relay=https%3A%2F%2Frelay.devpilot.app"
	if got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
	// relay 地址必须被转义,扫码端才能按 URL 解析。
	if !strings.Contains(got, "relay=https%3A%2F%2F") {
		t.Fatal("relay 未转义")
	}
}

func TestRenderPairQR(t *testing.T) {
	var out bytes.Buffer
	renderPairQR(&out, pairQRPayload("https://relay.devpilot.app", "ABC234567"))
	// 半块字符画:非空、多行、含 QR 半块字符。
	s := out.String()
	if len(s) < 100 || !strings.ContainsAny(s, "▀▄█") {
		t.Fatalf("二维码渲染异常: %q", s[:min(80, len(s))])
	}
}

// TestUnpairRevokesOnRelay unpair 应尽力调 DELETE /v1/daemon;
// relay 不可达(离线/未升级)不阻塞本地解绑。
func TestUnpairRevokesOnRelay(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		// 未升级的 relay 返回 404:仍应视为"吊销尽力了"。
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	cred := "dpd_dd_test_" + strings.Repeat("s", 32)
	paths := config.NewPaths(t.TempDir())
	cfg := config.Default()
	cfg.DaemonID = "dd_test"
	cfg.DaemonCredential = cred
	cfg.RelayURL = ts.URL
	if err := cfg.Save(paths); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runUnpair([]string{"--dir", paths.Dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/daemon" {
		t.Fatalf("吊销请求 = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer "+cred {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(stdout.String(), "已吊销") {
		t.Fatalf("stdout = %s", stdout.String())
	}

	// relay 离线:解绑仍应成功退出 0。
	paths2 := config.NewPaths(t.TempDir())
	cfg2 := config.Default()
	cfg2.DaemonID = "dd_test"
	cfg2.DaemonCredential = cred
	cfg2.RelayURL = "http://127.0.0.1:1" // 必然连接失败
	if err := cfg2.Save(paths2); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := runUnpair([]string{"--dir", paths2.Dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("relay 离线时 exit=%d", code)
	}
	saved, err := config.Load(paths2)
	if err != nil || saved.DaemonID != "" || saved.DaemonCredential != "" {
		t.Fatalf("离线解绑后配置 = %+v err=%v", saved, err)
	}
}
