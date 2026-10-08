// @vitest-environment node
import { describe, expect, it } from "vitest";
import { buildHtmlCard, extractHtmlFence, htmlRevisePrompt } from "./cards.js";

describe("extractHtmlFence", () => {
  it("extracts a single ```html block and returns the remaining text", () => {
    const text = [
      "好的，这是你要的页面：",
      "",
      "```html",
      "<h1>Hello</h1>",
      "```",
      "",
      "如需修改请告诉我。",
    ].join("\n");
    const { blocks, rest } = extractHtmlFence(text);
    expect(blocks).toEqual(["<h1>Hello</h1>"]);
    expect(rest).toContain("好的，这是你要的页面：");
    expect(rest).toContain("如需修改请告诉我。");
    expect(rest).not.toContain("<h1>");
  });

  it("extracts multiple html blocks and joins them via blocks[]", () => {
    const text = [
      "第一部分：",
      "```html",
      "<p>one</p>",
      "```",
      "第二部分：",
      "```html",
      "<p>two</p>",
      "```",
    ].join("\n");
    const { blocks, rest } = extractHtmlFence(text);
    expect(blocks).toEqual(["<p>one</p>", "<p>two</p>"]);
    expect(rest).toContain("第一部分：");
    expect(rest).toContain("第二部分：");
    expect(rest).not.toContain("<p>one</p>");
  });

  it("keeps non-html fences untouched", () => {
    const text = ["代码如下：", "```js", "console.log(1)", "```"].join("\n");
    const { blocks, rest } = extractHtmlFence(text);
    expect(blocks).toEqual([]);
    expect(rest).toContain("```js");
    expect(rest).toContain("console.log(1)");
  });

  it("supports ~~~ fences and indented fences", () => {
    const text = ["~~~html", "<b>x</b>", "~~~", "", "  ```html", "  <i>y</i>", "  ```"].join("\n");
    const { blocks } = extractHtmlFence(text);
    expect(blocks).toEqual(["<b>x</b>", "<i>y</i>"]);
  });

  it("returns empty blocks for plain text without fences", () => {
    const { blocks, rest } = extractHtmlFence("普通回复，没有代码块。");
    expect(blocks).toEqual([]);
    expect(rest).toBe("普通回复，没有代码块。");
  });

  it("handles html being the whole reply (empty rest)", () => {
    const text = "```html\n<p>only</p>\n```";
    const { blocks, rest } = extractHtmlFence(text);
    expect(blocks).toEqual(["<p>only</p>"]);
    expect(rest).toBe("");
  });

  it("tolerates an unclosed fence at end of text", () => {
    const text = "看这个：\n```html\n<p>unclosed</p>";
    const { blocks, rest } = extractHtmlFence(text);
    expect(blocks).toEqual(["<p>unclosed</p>"]);
    expect(rest).toContain("看这个：");
  });
});

describe("buildHtmlCard", () => {
  it("builds the card shape the web AiCommandCard renders", () => {
    const card = buildHtmlCard({ cardId: "card-1", markdown: "说明文字", html: "<h1>Hi</h1>" });
    expect(card.schema).toBe("1.0.0");
    expect(card.id).toBe("card-1");
    expect(card.header.title).toBe("HTML 预览");
    // markdown section carries the reply text minus fences
    expect(card.sections[0]).toMatchObject({ kind: "markdown", content: "说明文字" });
    // html section carries the raw html for the sandboxed preview
    expect(card.sections[1]).toMatchObject({ kind: "html", content: "<h1>Hi</h1>" });
    // input section wires the revise action the bridge understands
    const input = card.sections[2] as Record<string, Record<string, unknown>>;
    expect(input.kind).toBe("input");
    expect(input.submitButton).toMatchObject({
      id: "revise",
      label: "生成新版本",
      action: { type: "custom", kind: "html_revise", payload: {} },
    });
  });

  it("defaults the markdown section when the reply had no prose", () => {
    const card = buildHtmlCard({ cardId: "card-2", markdown: "", html: "<p>x</p>" });
    const md = card.sections[0] as { kind: string; content: string };
    expect(md.content).toContain("HTML");
  });
});

describe("htmlRevisePrompt", () => {
  it("embeds the previous html and the user instruction", () => {
    const prompt = htmlRevisePrompt("<p>old</p>", "换成蓝色");
    expect(prompt).toContain("```html");
    expect(prompt).toContain("<p>old</p>");
    expect(prompt).toContain("换成蓝色");
    expect(prompt).toContain("完整");
  });
});
