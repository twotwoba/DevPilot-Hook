> **副本说明**:本文为 DevPilot 仓库(`~/Desktop/DevPilot/Docs/`)的同步副本,源头与后续修订在 DevPilot 仓库。两项目关系见本仓库根 [README.md](../README.md)——mosh 线实现全部在 DevPilot(iOS)侧,与本仓库(daemon+relay)无实现交集。

# Mosh 客户端自研设计评审(兼容官方 mosh-server)

> 状态:评审稿 v1(2026-10-18)|性质:决策 + 设计文档,**本期不含实现**
> 前置文档:[Mosh决策门评审.md](Mosh决策门评审.md)(2026-09-19,无限期搁置)|关联:[Agent基建总纲与路线图.md](Agent基建总纲与路线图.md)

---

## 1. 背景与决策门重开

### 1.1 竞品拆解结论

Moshi(getmoshi.app)官方原文:"Native mosh Protocol — Moshi implements mosh directly, no shelling out to a subprocess"。其客户端不链接 GPL mosh 代码,而是净室重写 SSP 协议栈,与主机上**标准 mosh-server** 互通;mosh 传输是其 Pro 卖点("永不断线",Auto 模式优先 mosh → ET → SSH)。

### 1.2 决策门重开判定

[Mosh决策门评审.md](Mosh决策门评审.md) §4 写死了三条重开条件。逐条对照:

| # | 重开条件(原文) | 2026-10-18 现状 | 判定 |
|---|---|---|---|
| 1 | 用户实测弱网下「盯 + 审批」不可用,且 tmux 侧无改进空间 | 无用户报障;tmux + 自动重连持续达标 | ❌ 未触发 |
| 2 | 出现维护中的 Swift/跨平台 Mosh 协议库,评估成本 <2 周 | Spectty 出现(MIT、Swift 6、净室声明),但小项目、未审计、成本评估未达 <2 周 | ⚠️ 半触发 |
| 3 | 竞品用 Mosh 形成**不可替代的体验落差**且**用户流失可归因于此** | 体验落差已实证(Moshi 以此为核心卖点、demo 主打 Wake Probe);用户流失归因**尚无数据** | ⚠️ 半触发 |

**重开结论**:严格按条件 3 原文(落差 + 流失归因双要件)本次不算自动触发;本次重开是**产品负责人(用户)的主动战略决策**——依据是竞品体验落差已被实证、mosh-server 生态兼容具有战略防御价值,且用「M0 spike 成本闸门」替代原条件的观望等待:spike 不过,决策门再次关闭,沉没成本以人天计。此判定为对原决策门约定的**修订**,记录在案。

### 1.3 wire-compatible 的取舍(已拍板)

- **选兼容官方 mosh-server**:用户主机 `brew install mosh` / `apt install mosh` 即可,复用现成生态;客户端只需自研一端。
- 否决「自研自有协议 + 自家 daemon 双端」:两端都要养,总成本更高,且放弃「连任意装了 mosh 的 VPS」的用例。
- 范围:v1 目标 = 与官方 mosh-server **全流程互通**(bootstrap → SSP 同步 → 漫游 → 恢复);本地预测回显为 P2 可选(见 §14),与决策门记录的「唯一未达标项=弱网击键本地预测」对应。

---

## 2. mosh 协议栈全景拆解

> 信息源白名单内的公开资料(mosh 论文、man 手册、官方文档);字段级细节以 S5 抓包为准,本文不虚构协议内部结构。

### 2.1 连接生命周期

```
┌─ 阶段一:SSH bootstrap(复用 DevPilot 现有 SSHKit)──────────┐
│ SSH exec: mosh-server new [-p PORT[:PORT2]] [-i ADDR] [-- shell] │
│ 服务端输出: MOSH CONNECT <udp-port> <128bit-key>                  │
│ 解析哨兵行 → 主动关闭 SSH 通道(官方客户端同款行为)              │
├─ 阶段二:UDP 状态同步(自研核心)───────────────────────────┤
│ NWConnection(UDP) ↔ mosh-server                                   │
│ 每包: AES-128-OCB 加密,内含 SSP 序号化状态 diff                  │
│ 丢包无需 ARQ:更新的状态包天然覆盖旧包(状态同步范式)           │
│ 心跳保活;超时(默认 600s)服务端自杀退出                          │
├─ 阶段三:漫游与恢复 ─────────────────────────────────────────┤
│ IP/网络切换 → 客户端换源地址继续发包,服务端按密钥认证任意来源   │
│ iOS 后台挂起 → 回前台重绑端口、补发最新状态即恢复                 │
└──────────────────────────────────────────────────────────────┘
```

### 2.2 客户端必须实现的四层

| 层 | 职责 | 难点 | 本文对应节 |
|---|---|---|---|
| Bootstrap | SSH 拉起 mosh-server、解析 MOSH CONNECT | 现有 exec 通道会丢块(§8.2) | §8 |
| 加密 | AES-128-OCB 加解密、每包完整性 | 变体存疑(OCB1 vs RFC 7253);swift-crypto 无 OCB | §6 |
| SSP | 序号化状态同步、diff 编解码、心跳 | **无公开规范**(mosh issue #1087) | §7 |
| 终端状态 | 影子终端:SSP 接收端维护的屏幕状态模型 | SwiftTerm buffer 无公开提取 API → 自研 | §8.4 |

### 2.3 服务端侧用户须知(产品文案口径)

- 需安装 mosh-server(brew/apt),防火墙放行 UDP(默认 60000-61000);
- mosh 不承载端口转发/Agent 转发/SFTP——Git 等 SSH 功能仍走 DevPilot 原有 SSH 连接(两者并存,互不干扰);
- mosh 同步的是当前屏幕,久远滚动历史靠 tmux(DevPilot 本就以 tmux 为主干,天然互补);
- Tailscale 场景:系统级 VPN(含官方 Tailscale App)透明可用;**DevPilot 内嵌 Tailscale 桥当前纯 TCP,mosh 走 tailnet 依赖 S2 spike**(§9.3)。

---

## 3. 互通性目标与验收定义

### 3.1 wire-compatible 的判定标准(硬验收)

1. 与真实 mosh-server(Homebrew 1.4.x + Ubuntu apt 1.3.x)**全流程互通**:bootstrap → 屏幕同步 → 击键输入 → 网络切换漫游 → 杀 App 重进恢复;
2. 与官方 mosh-client **同服务器同会话互操作**:官方客户端接入我方拉起的会话、我方接入官方客户端拉起的会话,均正常;
3. 抓包对照:我方客户端与官方客户端对同一 mosh-server 的报文在结构层面可被同一套解码工具解析。

### 3.2 版本矩阵

| 目标 | 依据 |
|---|---|
| mosh 1.4.x(macOS Homebrew 主线)| 现网最新;OCB3 旁证见 §6.1 |
| mosh 1.3.x(Ubuntu/Debian apt 常见)| 兼容矩阵的另一端;SSP 行为若有版本差异在 M4 暴露并登记 |

---

## 4. 净室策略与法律卫生

### 4.1 红线

- **绝不阅读 mosh 官方 GPL 源码**——包括「只为理解协议」的阅读。GPL 污染经由「读源码 → 写实现」的衍生物主张路径成立,与是否逐字复制无关。
- 本仓库、本仓库 fork、开发机上的 mosh 源码副本均不得作为实现参照;评审/S1 spike 全部走黑盒(报文)路径。

### 4.2 信息源白名单

| 源 | 许可/性质 | 用途 |
|---|---|---|
| RFC 7253(OCB3) | IETF 规范 | 加密层实现依据 |
| Rogaway 原始 OCB 系列论文/规格 | 学术公开 | 若 S1 裁决为旧代 OCB 的实现依据 |
| mosh 论文(mosh.org/mosh-paper-draft.pdf)、man 手册、官方 FAQ | 公开文档 | SSP 语义理解 |
| **报文抓包**(对真实 mosh-server) | 黑盒行为 | SSP 字段级建模的唯一权威(S5) |
| Spectty(github.com/ocnc/spectty) | **MIT** | 交叉参照(边界见 §5) |

### 4.3 接触登记表(强制留痕)

凡接触 Spectty 或其他第三方 mosh 相关代码,逐次登记:

| 日期 | 人员 | 仓库/模块 | 接触目的 | 产出 |
|---|---|---|---|---|
| (模板) | | MoshSSP | 歧义仲裁 | 设计笔记链接 |

登记表随设计文档归档于本文档附录,进入实现期后作为合规证据链。**默认纪律:实现只依据 RFC/论文/抓包;第三方代码仅在自研版本与官方行为出现分歧时作仲裁参照,仲裁后仍以自研表达重写。**

---

## 5. Spectty 尽调(github.com/ocnc/spectty)

### 5.1 已核验事实

- **MIT 许可**(仓库页面标注);自称 "Clean-room Swift implementation — no GPL code";
- 四模块架构:MoshCrypto(自实现 AES-128-OCB3,基于 CommonCrypto AES-ECB)/ MoshSSP(序号化 diff + 心跳重传)/ MoshNetwork(Network.framework UDP 漫游)/ MoshBootstrap(SSH exec 解析 MOSH CONNECT);
- 已上架 App Store(App id 6759269910);规模:82 commits、42 stars、4 open issues、无 Release;要求 Xcode 26.2+/iOS 18+/Swift 6。

### 5.2 风险分级

| 风险 | 说明 | 等级 |
|---|---|---|
| 净室声明未经审计 | 若作者实为翻译 mosh C++ 逻辑,MIT 授权本身有效,但「无 GPL 污染」的前提可能不成立;我方依赖越深,污染传导越深 | 法律,阻塞级 |
| 与官方互通性未证实 | 声称 OCB3(RFC 7253),未见对官方 server 的互通测试记录 | 技术,阻塞级(与 S1 同题) |
| 项目成熟度 | 单人小项目,后续维护无承诺 | 一般 |

### 5.3 使用策略(已定)

1. **S3 尽调 spike 先行**(提交史溯源/作者背景/与 mosh 仓库结构相似度审计);
2. 默认策略 = **仅交叉参照**(§4.3 纪律),不做模块级依赖;
3. 若 S1 证实其加密层与官方互通 + S3 审计干净,可升级评估「参考其边界结论」(如密钥编码格式、包结构常量)——仍不抄代码,只核对行为;
4. 接触一律登记。

---

## 6. 加密层设计

### 6.1 变体裁决(S1 spike,阻塞级)

- 公开资料只说 "AES-128 in OCB mode",**未标明 OCB1/2/3 哪一代;各代互不互通**。
- 旁证:mosh 1.2.x 起发行说明曾提及切换到 OCB3(RFC 7253 前身草案),与 Spectty 声称一致;但现网 1.3/1.4 的真实行为以 S1 黑盒抓包终裁。
- OCB 专利已开放(2013 起许可开源使用、OCB3 于 2021 进入公有领域)——**专利不是障碍**;障碍仅是实现与净室流程。

### 6.2 实现路径

- swift-crypto 无 OCB(只有 AES-GCM/ChaChaPoly)→ 自实现:OCB3 按 RFC 7253 逐条实现(AES 分组原语可用 CryptoKit AES ECB 等价原语或 CommonCrypto),**以 RFC 7253 官方测试向量做单元测试金标准**;
- 若 S1 裁决为旧代 OCB:按 Rogaway 公开规格实现,同样用该规格自带向量;
- 产物:独立 `MoshCrypto` 目标(纯 Swift、无 IO),向量测试 + 对真实 server 的往返验证双层。

---

## 7. SSP 协议层设计

### 7.1 设计立场

SSP 无公开规范(mosh issue #1087 官方承认只有参考实现)→ **抓包 + 受控实验是唯一取证方法学**:

1. 自建 mosh-server(Docker 固定版本),构造已知终端操作序列(逐字符/清屏/光标移动/全屏应用 vim/resize);
2. 每步抓包,对 diff 编码建模:操作 → 线上字节的映射表;
3. 模型先实现为「解码器」(只读),跑通只读同步后再实现「编码器」(击键/resize 上行);
4. 版本矩阵(1.3/1.4 × linux/mac)各建一遍模型,差异登记。

### 7.2 设计要点(按公开语义)

- 双端各持对象状态 + 序号;**丢包免 ARQ**:后续包携带的新状态天然取代旧状态,重传只需重发更新的状态;
- 完整性校验随 OCB 认证标签一并解决;
- 心跳间隔与超时参数按官方客户端公开行为取值(600s 服务端超时),并在抓包中复核;
- 我方 SSP 层独立为 `MoshSSP` 目标:输入 = 加密前明文字节,输出 = 结构化状态更新,与网络/加密层解耦,便于向量测试与回放测试(六段真机抓包回放进测试的既有工程惯例可复用)。

---

## 8. DevPilot 架构集成

### 8.1 模块与传输双轨

- 新增 `Sources/MoshKit/`(SSP/加密/影子终端/UDP,纯逻辑,可单测)+ project.yml target 注册(xcodegen 工程,新模块必须落 glob 内);
- `MoshConnectionManager` actor 与 `SSHConnectionManager` **同接口**(connect/send/resize/disconnect);`App/SessionController.swift` 的 Session 增加传输枚举,`wireRemoteIO` 泵线不变——**MoshKit 与 SSHKit 是并列的「输出生产者」,TerminalDriver.feed 唯一渲染入口不动**;
- 状态机不新增状态:mosh 漫游复用 `reconnecting(attempt:)` 语义;现有断线退避(1s→2s→5s→10s×6)对 UDP 层退化为「重绑 + 补发」循环。

### 8.2 Bootstrap:新增 `execBootstrap` API(SSHKit)

现有 `SSHConnectionManager.runCaptured` promise 在通道关闭时才完成,且仓库已实证「连接初期 exec 丢块」(`Sources/CoreModels/TmuxSupport.swift` L66-70 注释)——**mosh 密钥丢一个字符即整段作废**。规划:

- 流式逐行读 stdout,命中 `MOSH CONNECT` 哨兵行即收口并主动关通道;
- 复用 TmuxProbe 的哨兵自校验 + `command -v` + 常见路径前缀遍历模式(mosh-server 不在 PATH 时逐个探测 `/opt/homebrew/bin` 等);
- 超时 + 密钥格式完整性校验 + 失败重试梯。

### 8.3 影子终端

SwiftTerm buffer 无公开文本提取 API(`Sources/TerminalKit/TerminalDriver.swift` L47-48 注释)→ **自研 SSP 接收端状态模型即影子终端**;副产品:任意时刻可导出屏幕快照,直接喂给「会话缩略图」「收件箱预览」「tmux 恢复选择器卡片」——这是 mosh 线对 Agent 值班台的隐性红利,实现期立项。短期兜底沿用 `CapturedLineBuffer`(纯文本行环)。

### 8.4 持久化与表单

- Keychain:`host.<uuid>.mosh` 账号(**必须 ThisDeviceOnly,不进 iCloud**——密钥绑定服务器端存活进程,跨设备同步无意义且扩大泄露面);`KeychainStore.deleteHostCredentials` 追加级联清理;v1 不承诺跨 App 重启复用会话(重连一律重新 bootstrap,与官方客户端一致);
- `HostRecord`(SwiftData):新增 `transportRaw: String?`、`moshServerPath: String?` ——**必须 Optional**(非可选新增曾致冷启 fatalError,`useTailscale: Bool?` 是先例),取值端回落 Auto;
- `HostFormView` 新插「传输」段(Auto/SSH/Mosh + mosh-server 自定义路径);UDP 端口范围为全局偏好,落 `AppPreferences`。

---

## 9. 网络层与漫游

### 9.1 iOS 网络层

- `NWConnection`(UDP),params 用 IPv4+IPv6;
- 网络路径变化(Network.framework path update)→ 触发重绑 + 立即补发最新状态(Wi-Fi↔蜂窝切换的核心恢复路径);
- 后台挂起 → 回前台 `scenePhase` 钩子触发重绑与补发(S4 spike 实测三档时延);
- **iOS 不给长后台承诺**:mosh 的优势叙事是「锁屏回来/切换网络无缝续上」,不是后台常驻——产品文案按此口径,与豆包拆解结论一致。

### 9.2 漫游语义

mosh 服务端按会话密钥认证任意来源包 → 客户端换 IP/端口无需重连;DevPilot 侧对应实现 = 「同一 MoshConnectionManager 会话内换本地端点」,不触碰 SSH 生命周期。

### 9.3 Tailscale 三种出路(S2 spike)

DevPilot 内嵌 Tailscale 桥(`Scripts/TailscaleBridge/api.go`)当前 **纯 TCP**(OpenForward = `net.Listen("tcp", ...)`;`Dial(ctx, "tcp", ...)`)——mosh 原生 UDP 到 100.x 地址今天完全走不通。出路:

| 出路 | 内容 | 代价 |
|---|---|---|
| a | 桥内验证 tsnet `Dial(ctx, "udp", …)` 支持 | 最小改动;随 tailscale 版本变动需钉版本 |
| b | 直接嵌 netstack/wgengine 做 UDP 端点转发 | 工程量大 |
| c | 桥内起 UDP relay(netstack UDP over TCP 隧道) | 折中;引入薄封装层 |

注意:**经 netstack 后漫游语义退化**(出口恒为本机 tailnet IP,mosh 的换源地址漫游用不上,但「后台挂起恢复/断网重连」语义保留)——文档与文案须如实区分。系统级 Tailscale/VPN 用户不受影响,UDP 原生透明。

---

## 10. 传输选择与回退

- 主机级三选一:**Auto / SSH / Mosh**(默认 Auto);
- Auto 探测链:SSH bootstrap 阶段探测 mosh-server 存在性(`command -v` 梯)→ 存在则拉起 UDP → UDP 可达性失败(超时)静默回落纯 SSH 会话;**回退对用户不可见,状态行标注当前传输**;
- App Store 审核:审核内网常封 UDP → Auto 的 SSH 回退即审核路径;提审备注模板:「Mosh 为可选传输,UDP 不可达时自动使用 SSH;测试账号默认 SSH 可完整体验」;
- mosh 与 SSH 并存:同一主机会话可同时持有一条 SSH 连接(Git/SFTP 类操作)与一条 mosh 会话(终端),UI 上互不干扰——与 Moshi 的做法一致(mosh 不承载 agent forwarding 的官方限制)。

---

## 11. 持久化与安全

见 §8.4。补充威胁面:

- mosh 会话密钥 = 该 UDP 会话的完整控制权 → Keychain ThisDeviceOnly 强制;内存中密钥缓冲用后即清;
- bootstrap 阶段密钥经由 SSH 加密通道传输,无新增明文暴露面;
- 设备丢失场景:密钥仅本机 Keychain;服务器侧会话随超时自杀;用户可在服务器 `pkill mosh-server` 兜底。

---

## 12. 风险清单

| # | 风险 | 等级 | 缓解/验证 | 对应 spike |
|---|---|---|---|---|
| R1 | OCB 变体不匹配 → 加密层整体返工 | 阻塞 | 黑盒差分裁决;RFC 向量金标准 | S1(3-5 天) |
| R2 | 内嵌 Tailscale 桥无 UDP → tailnet 主机 mosh 不可用 | 高 | 三出路裁决;最坏「写明限制,引导系统级 VPN」 | S2(2-3 天) |
| R3 | SSP 逆向不完备 → 特定版本不互通 | 高 | 受控实验建模 + 1.3/1.4 矩阵回归 | S5(随 M1) |
| R4 | Spectty 净室声明不实 → 污染传导 | 法律,阻塞 | 仅交叉参照纪律 + 接触登记 + 提交史审计 | S3(2-3 天) |
| R5 | iOS 后台挂起恢复时延不达标 | 中 | 三档实测;scenePhase 立即重绑 | S4(3 天) |
| R6 | 审核环境 UDP 被封 → 审核被拒 | 中 | Auto 静默回退 SSH + 提审备注模板 | 免 spike(§10) |
| R7 | SwiftTerm 无快照 API → 缩略图受限 | 低 | 自研影子终端即快照源 | 免 spike(§8.3) |
| R8 | mosh-server 部署门槛 → 用户上手流失 | 低 | 安装引导 + 防火墙诊断进连接错误翻译层(SSHFailureDescriber 范式) | 免 spike |

---

## 13. Spike 清单与判据

| # | 问题 | 方法 | 通过判据 | 时间盒 |
|---|---|---|---|---|
| S1 | mosh 线上加密是 OCB 哪一代 | 对真实 mosh-server 起会话抓首包,黑盒差分比对 RFC 7253 向量与旧代向量;**不读 mosh 源码、不看 Spectty 内部** | 我方 RFC 7253 实现(或旧代实现)能解密真实握手包 | 3-5 天 |
| S2 | 内嵌 Tailscale 桥能否转发 UDP | `api.go` 内试 `Dial(ctx,"udp",…)`;评估三出路 | 出路 a/b/c 之一可行,产出选型结论与漫游退化说明 | 2-3 天 |
| S3 | Spectty 可依赖度 | 提交史/作者/结构相似度审计 + 接触登记表建账 | 审计报告:风险分级结论 + 「仅交叉参照」边界确认 | 2-3 天 |
| S4 | 后台挂起恢复时延 | 真机后台 30s/3min/10min 三档回前台实测 | 10min 档回前台 ≤3s 恢复可交互(目标值,M0 后校准) | 3 天 |
| S5 | SSP diff 编码建模 | 受控实验法(§7.1) | 已知操作序列 → 建模表覆盖 ≥95% 常见操作,解码器对真实会话跑通 | 随 M1 |

**M0 闸门**:S1 + S3(阻塞项)+ S2 结论齐 → 决策门正式放行进入 M1;任一阻塞 spike 失败 → 决策门再次关闭,记录归档。

---

## 14. 里程碑与人周估算(量级判断,非承诺)

| 阶段 | 内容 | 人周 | 闸门 |
|---|---|---|---|
| M0 | S1/S2/S3/S4 spike + Spectty 尽调 | 1-1.5 | **决策门放行/关闭** |
| M1 | MoshCrypto(向量测试)+ MoshSSP(解码器→编码器,含 S5) | 3-4 | 与真实 server 解码互通 |
| M2 | UDP 网络层 + 漫游 + 后台恢复 + SSHKit execBootstrap | 2-2.5 | 真机全流程互通(单版本) |
| M3 | 架构集成(MoshConnectionManager/传输选择/表单/持久化)+ 影子终端 | 1.5-2 | DevPilot 内端到端 |
| M4 | 互通矩阵(1.3/1.4 × linux/mac)+ 与官方 client 互操作 + 修缺 | 1.5-2 | §3.1 硬验收全绿 |
| P2(可选) | 本地预测回显(mosh 体验灵魂;独立决策门,参照决策门记录「唯一未达标项」) | +1-2 | 弱网击键实测 |

**合计 ≈ 8-12 人周**(不含 P2)。

## 15. 开放问题

1. S1 若裁决为旧代 OCB,是否仍沿用 Spectty 的交叉参照边界(预期:是,边界不变);
2. mosh 会话与 DevPilot 收件箱(tmux 会话命名/Agent 检测)的会话身份对齐——tmux 在 mosh 内运行,现有 tmux-inside 探测在 mosh 线的等价物(M3 细化);
3. 内嵌 Tailscale + mosh 的漫游退化文案口径(S2 结论后定);
4. P2 预测回显的决策门时机(M4 上线后按弱网反馈再议)。

---

## 来源与依据

- Moshi 官方文档(getmoshi.app/docs:connections/introduction)与官方原文 "Native mosh Protocol";Moshi 竞品拆解另见 [Moshi-竞品交互分析.md](Moshi-竞品交互分析.md)
- mosh issue #1087(SSP 无规范确认);mosh 论文(mosh.org/mosh-paper-draft.pdf);RFC 7253(OCB3)
- Spectty:github.com/ocnc/spectty(MIT、净室声明、四模块、82 commits/42 stars、App Store id6759269910)
- DeepSeek/豆包竞品分析对话(2026-10-18,用户提供的两轮拆解:iOS 后台冻结/审核 UDP 拦截/净室先例/mosh 弱点口径)
- 仓库内部事实:`Sources/SSHKit/SSHConnectionManager.swift`(runCaptured)、`Sources/CoreModels/TmuxSupport.swift`(L66-70 丢块实证;L76-107 探针)、`Sources/TerminalKit/TerminalDriver.swift`(feed 入口;L47-48 buffer 无公开 API)、`Sources/CoreModels/ConnectionTypes.swift`、`App/SessionController.swift`(wireRemoteIO/Session)、`Sources/PersistenceKit/`(SessionRecordStore/KeychainStore/HostRecord)、`App/Views/HostFormView.swift`、`App/AppPreferences.swift`、`Scripts/TailscaleBridge/api.go`(L211/L263 纯 TCP 实证)
