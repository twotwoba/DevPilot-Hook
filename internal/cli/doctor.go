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
	fs.BoolVar(&asJSON, "json", false, "output as JSON")
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
			checks = append(checks, checkResult{"daemon_id", "fail", "not generated yet, run install first"})
		} else {
			checks = append(checks, checkResult{"daemon_id", "ok", cfg.DaemonID})
		}
		if cfg.DaemonCredential == "" {
			checks = append(checks, checkResult{"pairing", "warn",
				"not paired yet, run devpilot-hook pair (default hosted relay " + config.DefaultRelayURL + ")"})
		} else {
			checks = append(checks, checkResult{"pairing", "ok", "paired with " + cfg.RelayURL})
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
		return checkResult{"gateway", "warn", "local token not found (daemon may not be running yet)"}
	}
	req, err := http.NewRequest(http.MethodGet, statusURL(cfg)+"/v1/status", nil)
	if err != nil {
		return checkResult{"gateway", "fail", err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return checkResult{"gateway", "warn", "not running or unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return checkResult{"gateway", "fail", fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return checkResult{"gateway", "warn", "could not parse status response"}
	}
	return checkResult{"gateway", "ok", fmt.Sprintf("queue_depth=%v pending=%v",
		status["queue_depth"], status["pending_approvals"])}
}

// checkRelayHealth 顺带用 HTTP Date 估算时钟偏差（JWT iat 对时钟敏感）。
func checkRelayHealth(cfg *config.Config) checkResult {
	if cfg.RelayURL == "" {
		return checkResult{"relay", "warn", "relay not configured (written after pair, default " + config.DefaultRelayURL + ")"}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(strings.TrimRight(cfg.RelayURL, "/") + "/healthz")
	if err != nil {
		return checkResult{"relay", "warn", "unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return checkResult{"relay", "fail", fmt.Sprintf("/healthz HTTP %d", resp.StatusCode)}
	}
	return checkResult{"relay", "ok", cfg.RelayURL}
}

func checkClock(cfg *config.Config) checkResult {
	if cfg.RelayURL == "" {
		return checkResult{"clock", "warn", "relay not configured, clock skew unavailable (available after pair)"}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Head(strings.TrimRight(cfg.RelayURL, "/") + "/healthz")
	if err != nil {
		return checkResult{"clock", "warn", "could not read relay time"}
	}
	defer resp.Body.Close()
	serverTime, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return checkResult{"clock", "warn", "relay did not return a Date header"}
	}
	skew := time.Since(serverTime)
	if skew < 0 {
		skew = -skew
	}
	status := "ok"
	if skew > 30*time.Second {
		status = "warn"
	}
	return checkResult{"clock", status, "skew " + skew.Round(time.Second).String()}
}

func checkHooks(agentName string) checkResult {
	path, err := agentConfigPath(agentName)
	if err != nil {
		return checkResult{"hooks." + agentName, "fail", err.Error()}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return checkResult{"hooks." + agentName, "warn", "not installed (" + path + ")"}
	}
	if !strings.Contains(string(data), hookMarker+" "+agentName) {
		return checkResult{"hooks." + agentName, "warn", "devpilot-hook entry not found in agent config"}
	}
	return checkResult{"hooks." + agentName, "ok", path}
}

func checkQueue(paths config.Paths) checkResult {
	info, err := os.Stat(paths.WALPath())
	if err != nil {
		return checkResult{"queue", "ok", "empty (no events yet)"}
	}
	return checkResult{"queue", "ok", fmt.Sprintf("%d bytes", info.Size())}
}
