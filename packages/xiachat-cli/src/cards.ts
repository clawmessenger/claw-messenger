// HTML-card bridge helpers: detect fenced ```html blocks in agent replies
// and turn them into card_message / card_update payloads the web client
// renders with a sandboxed preview (AiCommandCard "html" section kind).

export interface HtmlFenceResult {
  // Extracted ```html fence bodies (trimmed).
  blocks: string[];
  // The reply text with all html fences removed (trimmed; may be empty).
  rest: string;
}

// Match fenced code blocks whose info string starts with "html" (html, html5,
// html+erb ...). Both ``` and ~~~ fences, 3+ chars, tolerate leading indent.
const FENCE_RE = /(^|\n)[ \t]*(`{3,}|~{3,})[ \t]*(html[^\n]*)?[ \t]*\n([\s\S]*?)(?:\n[ \t]*\2[ \t]*(?=\n|$)|$)/gi;

export function extractHtmlFence(text: string): HtmlFenceResult {
  if (!text) return { blocks: [], rest: "" };
  const blocks: string[] = [];
  const marker = "\u0000";
  let marked = text;
  // Two passes so we can strip fences from the remainder without guessing
  // offsets: first collect bodies, then replace each fence with a marker.
  marked = marked.replace(FENCE_RE, (match, lead: string, fence: string, info: string | undefined, body: string) => {
    if (!info || !/^html\b/i.test(info.trim())) {
      // Not an html fence; keep the original text untouched.
      return match;
    }
    blocks.push(body.trim());
    return `${lead}${marker}`;
  });
  const rest = marked
    .split(marker)
    .map((part) => part.replace(/^[ \t]*\n/, "").replace(/\n[ \t]*$/, ""))
    .join("\n")
    .trim();
  return { blocks, rest };
}

export interface HtmlCardShape {
  schema: string;
  id: string;
  header: { title: string; icon: string };
  sections: Array<Record<string, unknown>>;
}

export function buildHtmlCard(opts: { cardId: string; markdown: string; html: string }): HtmlCardShape {
  return {
    schema: "1.0.0",
    id: opts.cardId,
    header: { title: "HTML 预览", icon: "</>" },
    sections: [
      {
        kind: "markdown",
        content: opts.markdown || "已生成 HTML，可在下方预览、查看源码或下载。",
      },
      { kind: "html", content: opts.html },
      {
        kind: "input",
        label: "修改要求",
        placeholder: "想怎么改？例如：换成蓝色主题",
        multiline: true,
        submitButton: {
          id: "revise",
          label: "生成新版本",
          variant: "primary",
          action: { type: "custom", kind: "html_revise", payload: {} },
        },
      },
    ],
  };
}

export function htmlRevisePrompt(html: string, instruction: string): string {
  return [
    "你之前生成了一个 HTML 页面，用户要求修改。请根据修改要求输出修改后的完整 HTML。",
    "",
    "【上一版 HTML】",
    "```html",
    html,
    "```",
    "",
    `【修改要求】${instruction}`,
    "",
    "要求：",
    "1. 输出修改后的完整 HTML 文档（不要只输出差异）。",
    "2. 用一个 ```html 代码块包裹，不要在代码块外写多余解释。",
  ].join("\n");
}
