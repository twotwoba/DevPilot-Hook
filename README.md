# DevPilot-Hook

DevPilot 的 **hook 守护进程 + 推送中继**:让「AI Agent 等审批」在 DevPilot App 被杀、手机锁屏时依然可达,审批决策原路回流,Agent 不中断。

## 与 DevPilot 的关系(两仓库、一条链路)

| | DevPilot(`~/Desktop/DevPilot`)| DevPilot-Hook(本仓库)|
|---|---|---|
| 形态 | iOS App(Swift/SwiftUI)| Go 单二进制:常驻 daemon + 自建中继 relay |
| 职责 | 终端/SSH/mosh、收件箱 UI、审批面板、Live Activity、APNs device token 注册 | hook 适配(Claude Code/Codex)、事件聚合限流、本地网关、WSS、配对、APNs 推送、路由表 |
| 在链路中的位置 | WSS 客户端:下行收事件,上行审批决策 | 中枢:daemon 与 App 各持配对 token 接入 |
| 代码边界 | App 侧接入:RelayChannel 模型/云端通道页/收件箱/Live Activity push token 上报 | daemon、relay、协议类型库(Go module,线上协议是两端唯一契约)|

```
Claude Code/Codex hooks → daemon(本地网关 127.0.0.1:24543)→ WSS → relay → APNs → iPhone
                                                          ↑ 审批 Allow/Deny 沿同链路回流 ↓
```

设计要点(详见 Docs/):审批链路任何环节超时 = 无决策回落终端原生询问(只会多问一次,不会放行);云通道只传「通知里你看得到的」摘要白名单;对标竞品 moshi-hook。

## 当前状态

- ✅ 设计文档三份(2026-10-18,评审稿 v1)
- ⬜ D0–D5 实现未开工;本仓库无外部阻塞,可先行动工(排期见总纲)

## 文档(源头在 DevPilot 仓库 `Docs/`,此处为同步副本)

| 文档 | 与本仓库的关系 |
|---|---|
| [DevPilot-Hook守护进程与推送中继设计.md](Docs/DevPilot-Hook守护进程与推送中继设计.md) | **核心设计**:daemon/中继/APNs 原理/审批闭环,实现落本仓库 |
| [Agent基建总纲与路线图.md](Docs/Agent基建总纲与路线图.md) | 两线(mosh × daemon+relay)总排期、两仓库代码边界、商业化门控 |
| [Mosh客户端自研设计评审.md](Docs/Mosh客户端自研设计评审.md) | 参照:DevPilot 侧 mosh 线,与本仓库无实现交集;收件箱/Live Activity 等交叉点的 App 侧归 DevPilot 仓库 |
