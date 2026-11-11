package cli

import (
	"encoding/json"
	"fmt"
)

// openCodePluginVersion 是插件文件协议版本；升级插件内容时递增，
// uninstall/doctor 据此识别自家文件。
const openCodePluginVersion = 2

// openCodePluginMarker 是插件文件的身份标记（首行注释），uninstall
// 只删除带标记的文件，绝不碰用户自己的插件。
const openCodePluginMarker = "devpilot-hook managed plugin"

// openCodePluginName 是插件文件名。扩展名固定 .ts：
// OpenCode 0.x 的事件目录只扫 plugin/*.{ts,js}，2.x 扫 plugin|plugins/*.{ts,js}
// （.mjs 两边都不认——旧版插件写 .mjs 是错的）。
const openCodePluginName = "devpilot-hook.ts"

// openCodeVariant 区分 OpenCode 两代插件 API。
//
//   - v1（OpenCode 0.x）：目录 plugin/，一个模块的每个导出都是插件工厂
//     (input) => Promise<Hooks>，审批点 permission.ask。
//   - v2（OpenCode 2.x）：目录 plugins/（也扫 plugin/），默认导出
//     { id, setup(ctx) }，审批点 ctx.permission.hook("evaluate")。
//
// 1.x 仍沿用 v1 的 hooks 类型（permission.ask），按 v1 处理。
type openCodeVariant int

const (
	openCodeV1 openCodeVariant = iota
	openCodeV2
)

// String 返回变体的短名（日志/doctor 用）。
func (v openCodeVariant) String() string {
	if v == openCodeV2 {
		return "v2"
	}
	return "v1"
}

// pluginDir 返回变体对应的插件子目录名。
func (v openCodeVariant) pluginDir() string {
	if v == openCodeV2 {
		return "plugins"
	}
	return "plugin"
}

// openCodePluginSource 生成指定变体的插件源码。
//
// 插件职责极薄：把 OpenCode 的权限询问 / 会话事件组装成 Claude hook
// 输入同构的 JSON，喂给 EXE（devpilot-hook hook opencode），由 CLI 完成
// 配置解析、token 读取、网关寻址、tmux 探测与超时策略——插件不直接
// 访问网关，换端口/换 token 不需要重装插件。
//
// 决策语义：EXE stdout 输出 {"decision":"allow"|"deny"} 才改写裁决；
// 其余一切（EXE 缺失、超时、网关 204）保持原样，OpenCode 回落原生
// 权限对话框。
//
// 超时三明治（从内到外）：网关审批窗 approval_timeout_seconds（默认
// 1800s）→ CLI 客户端超时 +50s → 插件保险丝 +120s。保险丝从 config.json
// 读审批窗，用户自定义更长审批窗时三档自动对齐；读不到（状态目录被清）
// 回落默认 1800s。
func openCodePluginSource(v openCodeVariant, exe string) string {
	// EXE 路径恒以 JS 字符串字面量注入（json.Marshal 产出的转义规则与
	// JS 字面量兼容）。不能用 commandLine 的「含空格才加引号」规则——
	// 那是 shell 语境；JS 里裸路径会被解析成正则字面量直接语法错误。
	quoted, err := json.Marshal(exe)
	if err != nil {
		// string 的 Marshal 恒成功；防御分支走 fallback。
		quoted = []byte(`"devpilot-hook"`)
	}
	body := openCodePluginV1Body
	if v == openCodeV2 {
		body = openCodePluginV2Body
	}
	return fmt.Sprintf(`// %s v%d — do not edit.
// Reinstall: devpilot-hook install --agent opencode
// Remove:    devpilot-hook uninstall --agent opencode
%s
%s`, openCodePluginMarker, openCodePluginVersion, openCodePluginPrelude(quoted), body)
}

// openCodePluginPrelude 是两个变体共用的运行时：子进程调用与超时策略。
func openCodePluginPrelude(quotedExe []byte) string {
	return fmt.Sprintf(`import { spawn } from "node:child_process";
import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";

const EXE = %s;
// 遥测事件（会话边界 / 工具起止）的兜底超时：CLI 本地失败是毫秒级，
// 该上限只兜 EXE 卡死，不给工具链加可感知延迟。
const TELEMETRY_TIMEOUT_MS = 15000;
const DEFAULT_APPROVAL_SECONDS = 1800;

// configRoot 复刻 Go os.UserConfigDir：macOS 走 ~/Library/Application
// Support，其余平台 XDG_CONFIG_HOME 或 ~/.config（daemon 状态目录语义；
// OpenCode 自己的插件目录是另一套路径，不能混用）。
function configRoot() {
  if (process.platform === "darwin") return join(homedir(), "Library", "Application Support");
  return process.env.XDG_CONFIG_HOME || join(homedir(), ".config");
}

// 审批保险丝缓存：只读一次 config.json。
let approvalFuseMs = null;
async function approvalTimeoutMs() {
  if (approvalFuseMs !== null) return approvalFuseMs;
  let seconds = DEFAULT_APPROVAL_SECONDS;
  try {
    const cfg = JSON.parse(await readFile(join(configRoot(), "devpilot-hook", "config.json"), "utf8"));
    if (Number(cfg && cfg.approval_timeout_seconds) > 0) seconds = Number(cfg.approval_timeout_seconds);
  } catch {}
  approvalFuseMs = (seconds + 120) * 1000;
  return approvalFuseMs;
}

// callHook 起 EXE 子进程喂 JSON，收 stdout 上的决策；任何失败一律
// resolve(null)——插件绝不向 OpenCode 抛错，失败即回落原生询问。
function callHook(payload, timeoutMs) {
  return new Promise((resolve) => {
    let child;
    try {
      child = spawn(EXE, ["hook", "opencode"], { stdio: ["pipe", "pipe", "ignore"] });
    } catch {
      resolve(null);
      return;
    }
    let out = "";
    let done = false;
    const finish = (value) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      try { child.kill(); } catch {}
      resolve(value);
    };
    const timer = setTimeout(() => finish(null), timeoutMs);
    child.stdout.on("data", (chunk) => { out += chunk; });
    child.on("error", () => finish(null));
    child.on("close", () => {
      const text = out.trim();
      if (!text) { finish(null); return; }
      try { finish(JSON.parse(text)); } catch { finish(null); }
    });
    try { child.stdin.end(JSON.stringify(payload)); } catch { finish(null); }
  });
}

// permissionInput 从 v1 的 Permission 组装审批卡正文：metadata 是工具
// 实参（command / pattern 等），title 与 pattern 补上让卡片说人话。
function permissionInput(permission) {
  const input = Object.assign({}, (permission && permission.metadata) || {});
  if (permission && permission.title && input.title === undefined) input.title = permission.title;
  if (permission && permission.pattern !== undefined && input.pattern === undefined) input.pattern = permission.pattern;
  return input;
}

// applyV1Decision 把决策写回 v1 的 output.status；无法识别时保持 "ask"。
function applyV1Decision(output, decision) {
  if (!output || !decision) return;
  if (decision.decision === "allow") output.status = "allow";
  else if (decision.decision === "deny") output.status = "deny";
}
`, string(quotedExe))
}

// openCodePluginV1Body 是 OpenCode 0.x 的插件体：命名导出 +
// permission.ask / tool.execute.* / event 钩子。
const openCodePluginV1Body = `
// DevPilotHook 是 v1 插件入口（命名导出即插件）。
export const DevPilotHook = async ({ directory }) => {
  // 会话上下文：目录/标题供后续事件补 cwd / prompt（权限事件本身不带
  // 项目路径，审批卡与收件箱路由靠它）；插件启动目录兜底。
  const sessions = new Map();
  const started = new Set();
  const fallbackCwd = directory || "";
  const remember = (sid, cwd, title) => {
    if (!sid) return;
    const prev = sessions.get(sid) || {};
    sessions.set(sid, { cwd: cwd || prev.cwd || fallbackCwd, title: title || prev.title || "" });
  };
  const cwdOf = (sid, explicit) => explicit || (sessions.get(sid) || {}).cwd || fallbackCwd;
  // ensureStarted 惰性补 SessionStart：OpenCode 0.x 实测不广播
  // session.created，只在第一次见到会话 id 时上报一次，避免收件箱缺会话。
  const ensureStarted = async (sid, cwd, title) => {
    if (!sid || started.has(sid)) return;
    started.add(sid);
    remember(sid, cwd, title);
    await callHook({
      hook_event_name: "SessionStart",
      session_id: sid,
      cwd: cwdOf(sid, cwd),
      prompt: (sessions.get(sid) || {}).title || "",
    }, TELEMETRY_TIMEOUT_MS);
  };
  return {
    event: async ({ event }) => {
      try {
        const type = event && event.type;
        const props = (event && event.properties) || {};
        if (type === "session.created") {
          const info = props.info || {};
          remember(info.id, info.directory, info.title);
          await ensureStarted(info.id, info.directory, info.title);
        } else if (type === "session.idle") {
          const sid = props.sessionID || "";
          await ensureStarted(sid, "");
          await callHook({ hook_event_name: "Stop", session_id: sid, cwd: cwdOf(sid, "") }, TELEMETRY_TIMEOUT_MS);
        } else if (type === "session.deleted") {
          const info = props.info || {};
          const sid = info.id || "";
          await ensureStarted(sid, info.directory, info.title);
          sessions.delete(sid);
          started.delete(sid);
          await callHook({
            hook_event_name: "SessionEnd",
            session_id: sid,
            cwd: cwdOf(sid, info.directory),
          }, TELEMETRY_TIMEOUT_MS);
        } else {
          // 其余会话事件（session.updated / message.updated ...）只用来
          // 补齐会话上下文，不产生云端事件。
          const sid = props.sessionID || (props.info && props.info.id) || "";
          const info = props.info || {};
          await ensureStarted(sid, info.directory || "", info.title || "");
        }
      } catch {}
    },
    "tool.execute.before": async (input, output) => {
      try {
        const sid = input.sessionID || "";
        await ensureStarted(sid, "");
        await callHook({
          hook_event_name: "PreToolUse",
          session_id: sid,
          tool_name: input.tool || "",
          tool_use_id: input.callID || "",
          tool_input: (output && output.args) || {},
          cwd: cwdOf(sid, ""),
        }, TELEMETRY_TIMEOUT_MS);
      } catch {}
    },
    "tool.execute.after": async (input) => {
      try {
        const sid = input.sessionID || "";
        await ensureStarted(sid, "");
        await callHook({
          hook_event_name: "PostToolUse",
          session_id: sid,
          tool_name: input.tool || "",
          tool_use_id: input.callID || "",
          tool_input: (input && input.args) || {},
          cwd: cwdOf(sid, ""),
        }, TELEMETRY_TIMEOUT_MS);
      } catch {}
    },
    "permission.ask": async (permission, output) => {
      try {
        const sid = (permission && permission.sessionID) || "";
        await ensureStarted(sid, "");
        const decision = await callHook({
          hook_event_name: "PermissionRequest",
          session_id: sid,
          tool_name: (permission && permission.type) || "",
          tool_use_id: (permission && (permission.callID || permission.id)) || "",
          tool_input: permissionInput(permission),
          cwd: cwdOf(sid, ""),
          prompt: (sessions.get(sid) || {}).title || (permission && permission.title) || "",
        }, await approvalTimeoutMs());
        applyV1Decision(output, decision);
        // 其余一切保持 "ask"：OpenCode 弹原生权限对话框。
      } catch {}
    },
  };
};
`

// openCodePluginV2Body 是 OpenCode 2.x 的插件体：默认导出
// { id, setup(ctx) }，用 ctx.tool.hook / ctx.permission.hook /
// ctx.event.subscribe 注册。
//
// 注意 v2 的 evaluate 钩子对 allow 与 ask 都执行，只有 effect === "ask"
// 才需要手机介入；deny 是终态，钩子根本不会被调用。
const openCodePluginV2Body = `
// permissionID 给 v2 的权限评估生成稳定的 tool_use_id：优先用工具调用
// 的 source.id，否则按 session+action+resources 派生——同一请求重复评估
// 时审批 ID 不漂移。
function permissionID(event) {
  if (event && event.source && event.source.id) return String(event.source.id);
  const key = [(event && event.sessionID) || "", (event && event.action) || "",
    ((event && event.resources) || []).join(",")].join("|");
  let h = 0;
  for (let i = 0; i < key.length; i++) h = (h * 31 + key.charCodeAt(i)) | 0;
  return "oc_" + (h >>> 0).toString(16);
}

// DevPilotHook 是 v2 插件定义（默认导出，id 稳定即可被 OpenCode 识别）。
const DevPilotHook = {
  id: "devpilot-hook",
  async setup(ctx) {
    // 会话上下文：目录/标题供后续事件补 cwd / prompt。
    const sessions = new Map();
    const started = new Set();
    const fallbackCwd = (ctx.location && ctx.location.directory) || "";
    const remember = (sid, cwd, title) => {
      if (!sid) return;
      const prev = sessions.get(sid) || {};
      sessions.set(sid, { cwd: cwd || prev.cwd || fallbackCwd, title: title || prev.title || "" });
    };
    const cwdOf = (sid, explicit) => explicit || (sessions.get(sid) || {}).cwd || fallbackCwd;
    // ensureStarted 惰性补 SessionStart：v2 的 session.created 并不一定
    // 广播给插件，首次见到会话 id 时补一次即可。
    const ensureStarted = async (sid, cwd, title) => {
      if (!sid || started.has(sid)) return;
      started.add(sid);
      remember(sid, cwd, title);
      await callHook({
        hook_event_name: "SessionStart",
        session_id: sid,
        cwd: cwdOf(sid, cwd),
        prompt: (sessions.get(sid) || {}).title || "",
      }, TELEMETRY_TIMEOUT_MS);
    };

    // 事件流：会话边界 → 遥测 / Stop。订阅放后台跑，绝不阻塞 setup。
    (async () => {
      for await (const event of ctx.event.subscribe()) {
        try {
          const type = event && event.type;
          const data = (event && event.data) || {};
          const sid = data.sessionID || "";
          const cwd = (event && event.location && event.location.directory) ||
            (data.location && data.location.directory) || "";
          if (type === "session.created") {
            remember(sid, cwd, data.title);
            await ensureStarted(sid, cwd, data.title);
          } else if (type === "session.idle") {
            await ensureStarted(sid, cwd);
            await callHook({ hook_event_name: "Stop", session_id: sid, cwd: cwdOf(sid, "") }, TELEMETRY_TIMEOUT_MS);
          } else if (type === "session.deleted") {
            await ensureStarted(sid, cwd);
            sessions.delete(sid);
            started.delete(sid);
            await callHook({ hook_event_name: "SessionEnd", session_id: sid, cwd: cwdOf(sid, cwd) }, TELEMETRY_TIMEOUT_MS);
          } else if (sid) {
            remember(sid, cwd, "");
            await ensureStarted(sid, cwd);
          }
        } catch {}
      }
    })().catch(() => {});

    await ctx.tool.hook("execute.before", async (event) => {
      try {
        const sid = event.sessionID || "";
        await ensureStarted(sid, "");
        await callHook({
          hook_event_name: "PreToolUse",
          session_id: sid,
          tool_name: event.tool || "",
          tool_use_id: String(event.id || ""),
          tool_input: event.input || {},
          cwd: cwdOf(sid, ""),
        }, TELEMETRY_TIMEOUT_MS);
      } catch {}
    });

    await ctx.tool.hook("execute.after", async (event) => {
      try {
        const sid = event.sessionID || "";
        await ensureStarted(sid, "");
        await callHook({
          hook_event_name: "PostToolUse",
          session_id: sid,
          tool_name: event.tool || "",
          tool_use_id: String(event.id || ""),
          tool_input: event.input || {},
          cwd: cwdOf(sid, ""),
        }, TELEMETRY_TIMEOUT_MS);
      } catch {}
    });

    await ctx.permission.hook("evaluate", async (event) => {
      // 配置规则已放行的调用不打扰手机；deny 是终态，钩子不会被调用。
      if (!event || event.effect !== "ask") return;
      try {
        const sid = event.sessionID || "";
        await ensureStarted(sid, "");
        const input = { resources: event.resources || [] };
        if (event.metadata) Object.assign(input, event.metadata);
        const decision = await callHook({
          hook_event_name: "PermissionRequest",
          session_id: sid,
          tool_name: event.action || "",
          tool_use_id: permissionID(event),
          tool_input: input,
          cwd: cwdOf(sid, ""),
          prompt: (sessions.get(sid) || {}).title || "",
        }, await approvalTimeoutMs());
        if (decision && decision.decision === "allow") {
          event.effect = "allow";
        } else if (decision && decision.decision === "deny") {
          event.effect = "deny";
          event.message = decision.message || "Denied from phone";
        }
        // 其余一切保持 "ask"：OpenCode 弹原生权限对话框。
      } catch {}
    });
  },
};

export default DevPilotHook;
`
