package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail,omitempty"`
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir string
	var asJSON bool
	pathsFlag(fs, &dir)
	fs.BoolVar(&asJSON, "json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	paths, err := resolvePaths(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	checks := []checkResult{}
	cfg, err := config.Load(paths)
	if err != nil {
		checks = append(checks, checkResult{"config", "fail", err.Error()})
	} else {
		checks = append(checks, checkResult{"config", "ok", paths.ConfigPath()})
	}

	if cfg != nil {
		if cfg.DaemonID == "" {
			checks = append(checks, checkResult{"daemon_id", "fail", "尚未生成，请运行 install"})
		} else {
			checks = append(checks, checkResult{"daemon_id", "ok", cfg.DaemonID})
		}
		if cfg.DaemonCredential == "" {
			checks = append(checks, checkResult{"pairing", "warn", "尚未配对，运行 pair --relay <url>"})
		} else {
			checks = append(checks, checkResult{"pairing", "ok", "已配对 " + cfg.RelayURL})
		}
		checks = append(checks, checkClock(cfg))
		checks = append(checks, checkGateway(cfg, paths))
		checks = append(checks, checkRelayHealth(cfg))
	}

	checks = append(checks, checkHooks("claude"), checkHooks("codex"))
	checks = append(checks, checkQueue(paths))

	failed := false
	for _, c := range checks {
		if c.Status == "fail" {
			failed = true
		}
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"ok": !failed, "checks": checks})
	} else {
		for _, c := range checks {
			symbol := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}[c.Status]
			fmt.Fprintf(stdout, "%s %-12s %s\n", symbol, c.Name, c.Detail)
		}
	}
	if failed {
		return 1
	}
	return 0
}

// checkGateway 通过本地 token 查询运行中的网关。
func checkGateway(cfg *config.Config, paths config.Paths) checkResult {
	token, err := os.ReadFile(paths.TokenPath())
	if err != nil {
		return checkResult{"gateway", "warn", "本地 token 不存在（daemon 可能尚未启动）"}
	}
	req, err := http.NewRequest(http.MethodGet, statusURL(cfg)+"/v1/status", nil)
	if err != nil {
		return checkResult{"gateway", "fail", err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return checkResult{"gateway", "warn", "未运行或无法连接: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return checkResult{"gateway", "fail", fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return checkResult{"gateway", "warn", "状态响应无法解析"}
	}
	return checkResult{"gateway", "ok", fmt.Sprintf("queue_depth=%v pending=%v",
		status["queue_depth"], status["pending_approvals"])}
}

// checkRelayHealth 顺带用 HTTP Date 估算时钟偏差（JWT iat 对时钟敏感）。
func checkRelayHealth(cfg *config.Config) checkResult {
	if cfg.RelayURL == "" {
		return checkResult{"relay", "warn", "未配置 relay"}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(strings.TrimRight(cfg.RelayURL, "/") + "/healthz")
	if err != nil {
		return checkResult{"relay", "warn", "无法连接: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return checkResult{"relay", "fail", fmt.Sprintf("/healthz HTTP %d", resp.StatusCode)}
	}
	return checkResult{"relay", "ok", cfg.RelayURL}
}

func checkClock(cfg *config.Config) checkResult {
	if cfg.RelayURL == "" {
		return checkResult{"clock", "warn", "未配置 relay，无法校验时钟"}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Head(strings.TrimRight(cfg.RelayURL, "/") + "/healthz")
	if err != nil {
		return checkResult{"clock", "warn", "无法读取 relay 时间"}
	}
	defer resp.Body.Close()
	serverTime, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return checkResult{"clock", "warn", "relay 未返回 Date 头"}
	}
	skew := time.Since(serverTime)
	if skew < 0 {
		skew = -skew
	}
	status := "ok"
	if skew > 30*time.Second {
		status = "warn"
	}
	return checkResult{"clock", status, "偏差 " + skew.Round(time.Second).String()}
}

func checkHooks(agentName string) checkResult {
	path, err := agentConfigPath(agentName)
	if err != nil {
		return checkResult{"hooks." + agentName, "fail", err.Error()}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return checkResult{"hooks." + agentName, "warn", "未安装（" + path + "）"}
	}
	if !strings.Contains(string(data), hookMarker+" "+agentName) {
		return checkResult{"hooks." + agentName, "warn", "配置中未找到 devpilot-hook 条目"}
	}
	return checkResult{"hooks." + agentName, "ok", path}
}

func checkQueue(paths config.Paths) checkResult {
	info, err := os.Stat(paths.WALPath())
	if err != nil {
		return checkResult{"queue", "ok", "空（尚未产生事件）"}
	}
	return checkResult{"queue", "ok", fmt.Sprintf("%d 字节", info.Size())}
}
