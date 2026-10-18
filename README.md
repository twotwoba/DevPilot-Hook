# DevPilot-Hook

DevPilot 的 **hook 守护进程**:跑在用户开发机上的 Go 常驻进程,把 Claude Code/Codex 的 hooks 事件(含审批请求)送上云端推送链路,并把审批决策接回 Agent——App 被杀、手机锁屏,审批照样闭环。

## 三项目关系(两 Go 一 iOS,一条链路)

| | DevPilot(`~/Desktop/DevPilot`)| **DevPilot-Hook(本仓库)** | DevPilot-APNS(`~/Desktop/DevPilot-APNS`)|
|---|---|---|---|
| 形态 | iOS App(Swift/SwiftUI)| **Go 常驻 daemon(用户开发机)** | Go relay(云 VPS)|
| 职责 | 终端/SSH/mosh、收件箱 UI、审批面板、Live Activity、APNs token 注册 | hook 适配(Claude Code/Codex)、本地网关 127.0.0.1:24543、事件聚合限流、离线缓冲 | WSS 接入、配对路由表、APNs 推送、Live Activity 远程更新转发 |
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

## 当前状态

- ✅ 设计文档(2026-10-18,评审稿 v1)
- ⬜ 实现未开工:本仓库承担 D1/D2/D5(daemon 侧);`pkg/wire` 与 relay(R0-R3)在 DevPilot-APNS。两仓库均无外部阻塞,可并行开工

## 文档(源头在 DevPilot 仓库 `Docs/`,此处为同步副本)

| 文档 | 与本仓库的关系 |
|---|---|
| [DevPilot-Hook守护进程与推送中继设计.md](Docs/DevPilot-Hook守护进程与推送中继设计.md) | **核心设计**:daemon 侧实现落本仓库;中继章节为概要,实现级细化在 DevPilot-APNS |
| [Agent基建总纲与路线图.md](Docs/Agent基建总纲与路线图.md) | mosh × daemon+relay 两线总排期与三仓库代码边界 |
| DevPilot-APNS `Docs/中继服务端与APNs推送设计.md` | 中继实现级设计(`pkg/wire`/WSS 协议/APNs 客户端),见姊妹仓库 |
| [Mosh客户端自研设计评审.md](Docs/Mosh客户端自研设计评审.md) | 参照:DevPilot 侧 mosh 线,与本仓库无实现交集 |
