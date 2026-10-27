# DevPilot-Hook

DevPilot 的 **hook 守护进程**:跑在用户开发机上的 Go 常驻进程,把 Claude Code/Codex 的 hooks 事件(含审批请求)送上云端推送链路,并把审批决策接回 Agent——App 被杀、手机锁屏,审批照样闭环。

## 三项目关系(两 Go 一 iOS,一条链路)

| | DevPilot(`~/Desktop/DevPilot`)| **DevPilot-Hook(本仓库)** | DevPilot-APNS(`~/Desktop/DevPilot-APNS`)|
|---|---|---|---|
| 形态 | iOS App(Swift/SwiftUI)| **Go 常驻 daemon(用户开发机)** | Go relay(云 VPS)|
| 职责 | 终端/SSH、收件箱 UI、审批面板、Live Activity、APNs token 注册 | hook 适配(Claude Code/Codex)、本地网关 127.0.0.1:24543、事件聚合限流、离线缓冲 | WSS 接入、配对路由表、APNs 推送、Live Activity 远程更新转发 |
| 链路角色 | WSS 客户端(App 端):收推送、上行审批决策 | WSS 客户端(daemon 端):上行事件、收审批决策 | 中枢:鉴权、路由、APNs |
| 代码边界 | App 侧接入(RelayChannel 模型等)| daemon 本体;`pkg/wire` 经 go.mod 引用 DevPilot-APNS | relay 全部 + **`pkg/wire` 协议类型库(三端线上协议的单一事实源)** |

```
Claude Code/Codex hooks
  → DevPilot-Hook daemon(本仓库,本地网关 127.0.0.1:24543)
  → DevPilot-APNS relay(VPS:鉴权 → 路由表 → 选通道)
  → Apple APNs(HTTP/2)──或──App WSS 在线直推
  → iPhone(通知 / Live Activity / 收件箱)
审批回流:iPhone Allow/Deny → relay → daemon(本仓库)→ hook 解除阻塞,Agent 继续
```

设计要点:审批链路任何环节超时 = 无决策回落终端原生询问(只会多问一次,不会放行);云通道只传「通知里你看得到的」摘要白名单;对标竞品 moshi-hook。

## 安装

只支持 **macOS 与 Linux**（与 moshi-hook 的范围一致，不做 Windows）。

```bash
# macOS（Homebrew，与 moshi 相同的 tap + install 两步）
brew tap twotwoba/devpilot
brew install --cask devpilot-hook

# 也可以一条命令（Homebrew 会自动 clone tap）
brew install --cask twotwoba/devpilot/devpilot-hook

# macOS / Linux（脚本：校验 SHA-256，装到 ~/.local/bin）
curl -fsSL https://raw.githubusercontent.com/twotwoba/DevPilot-Hook/main/install.sh | sh
```

> `twotwoba/devpilot` 是第三方 tap，对应 GitHub 仓库
> `twotwoba/homebrew-devpilot`；它不在 Homebrew 官方仓库里。moshi 的
> `rjyo/moshi` 同理，对应 `rjyo/homebrew-moshi`。tap 只需添加一次，
> 之后 `brew upgrade --cask devpilot-hook` 即可。

Linux 还可以直接下载 Releases 里的 `.deb` / `.rpm` / `.tar.gz`。

```bash
devpilot-hook install            # 写入 Claude Code / Codex hooks（只增改自家条目）
devpilot-hook service install    # launchd / systemd user 自启
devpilot-hook pair               # 缺省用官方托管中继 relay.devpilot.app；终端打印二维码 + 配对码
devpilot-hook pair --relay https://relay.example.com   # 自部署中继时显式指定（双轨）
devpilot-hook doctor             # 配置、hooks、网关、relay、队列自检
devpilot-hook serve              # 前台运行；service 管理器也会执行它
```

`pair` 的地址回退链:`--relay` > config.json 的 `relay_url` > 官方托管中继
(`config.DefaultRelayURL`)。iPhone 端 DevPilot App 扫终端二维码即可完成配对
(payload 为 `devpilot://pair?v=1&relay=…&code=…`),也可手动输入配对码;
`--no-qr` 关闭二维码。`unpair` 会尽力调用 relay 的吊销端点级联删除云端数据,
失败(离线/旧版 relay)不影响本地解绑。

`uninstall` 只删除自家 hooks 条目与自启配置，`--purge` 才会清空状态目录。

## 开发与测试

```bash
cp go.work.example go.work   # 需要同级 DevPilot-APNS 源码
go test -race ./...
go test -race -tags=e2e ./test/e2e -v   # 需要同级 DevPilot-APNS，现场编译两个二进制
make build                    # ./bin/devpilot-hook
```

E2E 覆盖：真实 `hook` 子进程 → daemon → relay（APNs Mock）→ 假 App WSS，
验证 allow / deny 回流、重复决策 first-wins、超时无输出（绝不超时放行）、WAL 清空。

## 发布

本仓库依赖 DevPilot-APNS 的 `pkg/wire`，发布前必须先给姊妹仓库打 tag：

```bash
# 1. DevPilot-APNS：先发布 v0.1.0（relay + pkg/wire）
# 2. 本仓库：把校验和写入 go.sum（工作区的 replace 不参与此步骤）
GOWORK=off go mod tidy
# 3. 打 tag 触发 .github/workflows/release.yml（GoReleaser）
git tag v0.1.0 && git push origin v0.1.0
```

GoReleaser 只需要一个额外 Secret：`HOMEBREW_TAP_TOKEN`（推
`twotwoba/homebrew-devpilot`）。先建好这个空仓库，并给 PAT 授予
`Contents: Read and write`。

发布使用非 draft release：cask 清单引用 release 资产，草案期间外部下载
会 404。

## 当前状态

- ✅ daemon 实现：Claude Code / Codex 适配器、本地网关（127.0.0.1 + Bearer + 拒绝 Origin）、
  审批阻塞与决策回流、聚合限流、JSONL WAL（fsync / 重放 / 压实）、配对、
  doctor、macOS/Linux 自启
- ✅ 端到端测试（`-tags=e2e`）与 macOS/Linux CI 矩阵
- ✅ 分发：GoReleaser（Homebrew Cask / deb+rpm）、`install.sh`
- ⬜ iOS 侧 L3 接入（下一轮）
- ⬜ 真机 APNs sandbox 验证（需在 Apple Developer 创建 `.p8` 后执行）

## 文档(源头在 DevPilot 仓库 `Docs/`,此处为同步副本)

| 文档 | 与本仓库的关系 |
|---|---|
| [DevPilot-Hook守护进程与推送中继设计.md](Docs/DevPilot-Hook守护进程与推送中继设计.md) | **核心设计**:daemon 侧实现落本仓库;中继章节为概要,实现级细化在 DevPilot-APNS |
| [Agent基建总纲与路线图.md](Docs/Agent基建总纲与路线图.md) | daemon/relay 线总排期与三仓库代码边界 |
| DevPilot-APNS `Docs/中继服务端与APNs推送设计.md` | 中继实现级设计(`pkg/wire`/WSS 协议/APNs 客户端),见姊妹仓库 |
