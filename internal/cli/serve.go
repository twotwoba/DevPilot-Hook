package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/agent"
	"github.com/twotwoba/DevPilot-Hook/internal/agent/claude"
	"github.com/twotwoba/DevPilot-Hook/internal/agent/codex"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
	"github.com/twotwoba/DevPilot-Hook/internal/event"
	"github.com/twotwoba/DevPilot-Hook/internal/gateway"
	"github.com/twotwoba/DevPilot-Hook/internal/queue"
	"github.com/twotwoba/DevPilot-Hook/internal/relayclient"
)

func runServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir string
	var foreground bool
	pathsFlag(fs, &dir)
	fs.BoolVar(&foreground, "foreground", false, "前台运行（service 管理器使用；当前实现始终前台）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = foreground

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
	if cfg.EnsureIdentity() {
		if err := cfg.Save(paths); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	token, err := config.LoadOrCreateToken(paths)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	wal, err := queue.Open(paths.WALPath(), queue.Options{})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer wal.Close()

	reg := event.NewRegistry()
	adapters := map[wire.Agent]agent.Adapter{
		wire.AgentClaude: claude.Adapter{},
		wire.AgentCodex:  codex.Adapter{},
	}

	var publisher gateway.Publisher
	var relay *relayclient.Client
	if cfg.RelayURL != "" && cfg.DaemonCredential != "" {
		relay, err = relayclient.New(relayclient.Config{
			RelayURL:   cfg.RelayURL,
			Credential: cfg.DaemonCredential,
			DaemonID:   cfg.DaemonID,
			WAL:        wal,
			Registry:   reg,
		})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		publisher = relay
	}

	timeout := time.Duration(cfg.ApprovalTimeout) * time.Second
	gw, err := gateway.New(gateway.Params{
		Addr:              fmt.Sprintf("127.0.0.1:%d", cfg.GatewayPort),
		Token:             token,
		DaemonID:          cfg.DaemonID,
		ApprovalTimeout:   timeout,
		ToolEventInterval: time.Duration(cfg.ToolEventInterval) * time.Second,
		ApprovalMode:      config.ApprovalMode(cfg.ApprovalMode),
		Adapters:          adapters,
		Registry:          reg,
		WAL:               wal,
		Publisher:         publisher,
		ExtraStatus: func() map[string]any {
			if relay == nil {
				return map[string]any{"relay_configured": false}
			}
			return map[string]any{"relay_configured": true, "relay": relay.Status()}
		},
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go janitor(ctx, reg)

	errCh := make(chan error, 2)
	go func() { errCh <- gw.ListenAndServe(ctx) }()
	if relay != nil {
		go func() { errCh <- relay.Run(ctx) }()
	}

	select {
	case <-ctx.Done():
		fmt.Fprintln(stdout, "正在退出…")
		return 0
	case err := <-errCh:
		if err == nil {
			return 0
		}
		// 端口被占用时退回随机端口，并把实际端口写回配置。
		if isAddrInUse(err) {
			fmt.Fprintf(stderr, "端口 %d 被占用，改用随机端口\n", cfg.GatewayPort)
			return serveOnRandomPort(ctx, cfg, paths, token, wal, reg, adapters, publisher, timeout, stdout, stderr)
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
}

// serveOnRandomPort 是端口冲突时的退化路径。
func serveOnRandomPort(
	ctx context.Context, cfg *config.Config, paths config.Paths, token string,
	wal *queue.WAL, reg *event.Registry, adapters map[wire.Agent]agent.Adapter,
	publisher gateway.Publisher, timeout time.Duration, stdout, stderr io.Writer,
) int {
	gw, err := gateway.New(gateway.Params{
		Addr:              "127.0.0.1:0",
		Token:             token,
		DaemonID:          cfg.DaemonID,
		ApprovalTimeout:   timeout,
		ToolEventInterval: time.Duration(cfg.ToolEventInterval) * time.Second,
		ApprovalMode:      config.ApprovalMode(cfg.ApprovalMode),
		Adapters:          adapters,
		Registry:          reg,
		WAL:               wal,
		Publisher:         publisher,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	errCh := make(chan error, 1)
	go func() { errCh <- gw.ListenAndServe(ctx) }()

	// 等待监听完成，把实际端口写回配置，hook 子命令才能找到网关。
	deadline := time.Now().Add(2 * time.Second)
	for gw.Addr() == "127.0.0.1:0" || gw.Addr() == "" {
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, "等待随机端口监听超时")
			return 1
		}
		select {
		case err := <-errCh:
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			return 0
		case <-time.After(20 * time.Millisecond):
		}
	}
	if _, portStr, err := net.SplitHostPort(gw.Addr()); err == nil {
		if port, err := strconv.Atoi(portStr); err == nil {
			cfg.GatewayPort = port
			if err := cfg.Save(paths); err != nil {
				fmt.Fprintf(stderr, "写回网关端口失败: %v\n", err)
			} else {
				fmt.Fprintf(stdout, "网关已切换到 127.0.0.1:%d\n", port)
			}
		}
	}

	if err := <-errCh; err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func janitor(ctx context.Context, reg *event.Registry) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	cleanup := 0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			reg.Expire(now)
			cleanup++
			if cleanup%300 == 0 {
				reg.Cleanup(now, 10*time.Minute)
			}
		}
	}
}

// isAddrInUse 用错误文本判断端口冲突，避免依赖各平台不同的 errno 常量。
func isAddrInUse(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "address already in use") ||
		strings.Contains(s, "only one usage of each socket address")
}

// statusURL 返回本机网关地址。
func statusURL(cfg *config.Config) string {
	return "http://127.0.0.1:" + strconv.Itoa(cfg.GatewayPort)
}
