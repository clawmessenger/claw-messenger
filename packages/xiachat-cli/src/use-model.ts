import { splitModelRoute } from "./agents.js";

// web 切模型发的是**一条普通私聊文本** `/use-model <provider>/<model>`
// （clawmessenger-web/src/pages/Home.tsx handleConversationModelChange）。
// 在这套 CLI 补上这个解析之前，那条文本会被当成用户提问送进 agent —— 用户点
// 「角色模型」，模型回一句「我不认识这个命令」，或者干脆开始执行一个叫
// use-model 的任务。
//
// 这里只做解析，不碰存储：存储见 user-models.ts（按用户持久化）。
export type UseModelCommand =
  | { kind: "query" }
  | { kind: "clear" }
  | { kind: "set"; route: string }
  | { kind: "invalid"; arg: string };

// 只认带斜杠的 `/use-model`：那是 web 的文本命令格式。openclaw 插件里另有
// 一个不带斜杠的 `use-model ` 前缀，但那是卡片按钮的 action name，不是进来
// 的聊天文本 —— 在这里放宽成「也认不带斜杠」会把「use model 是什么意思」
// 这类正常提问吃掉。
const PREFIX = "/use-model";

// 「恢复节点默认」的几种写法。auto / default 是用户直觉里最常用的词，
// 留着它们比报错有帮助。
const RESET_WORDS = new Set(["reset", "default", "clear", "none", "auto", "-", "off"]);

export function parseUseModelCommand(text: string): UseModelCommand | null {
  const trimmed = text.trim();
  if (!trimmed.startsWith(PREFIX)) return null;
  const rest = trimmed.slice(PREFIX.length);
  // `/use-models` `/use-modelx` 是别的词，不是本命令：前缀后面必须紧跟空白
  // 或直接结束。
  if (rest && !/^\s/.test(rest)) return null;

  const arg = rest.trim();
  if (!arg) return { kind: "query" };
  if (RESET_WORDS.has(arg.toLowerCase())) return { kind: "clear" };

  // route 必须是 `provider/model`。裸 slug 在这套 CLI 里没有唯一含义 ——
  // codex 要 `-c model_provider=` 配 provider、hermes 要 `--provider`、qwen
  // 只吃裸名（见 agents.ts buildModelArgs）—— 只有写全 route，每种 agent 才
  // 都能被还原成正确的 argv。所以这里拒收裸 slug，并明确告诉用户怎么写。
  const parsed = splitModelRoute(arg);
  if (!parsed) return { kind: "invalid", arg };
  return { kind: "set", route: arg };
}

/**
 * 命令回执。走普通私聊文本，不进 agent —— 回执必须在一秒内到达，而 spawn 一个
 * agent 冷启动就要好几秒。
 *
 * 文案刻意写明「仅对你生效」：同一台节点可能同时服务多个人 / 多个讨论组角色，
 * 用户很容易以为自己在改整台设备的模型。
 */
export function formatUseModelAck(command: UseModelCommand, current: string | undefined): string {
  switch (command.kind) {
    case "set":
      return `已切换模型：${command.route}\n仅对你在此节点上的会话生效，下一轮开始使用。`;
    case "clear":
      return "已恢复节点默认模型，仅对你在此节点上的会话生效。";
    case "invalid":
      return `模型要写成 <provider>/<model>，例如 quukk/glm-5.3。收到的是「${command.arg}」。`;
    case "query":
      return current
        ? `当前模型：${current}\n发送 /use-model default 可恢复节点默认。`
        : "当前使用节点默认模型。\n发送 /use-model <provider>/<model> 切换，例如 /use-model quukk/glm-5.3。";
  }
}
