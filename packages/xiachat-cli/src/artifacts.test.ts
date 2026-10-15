import { mkdtempSync, mkdirSync, writeFileSync, rmSync, utimesSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { collectProducedArtifacts, isPreviewableArtifact, snapshotWorkdir } from "./artifacts.js";

let root = "";

beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), "cm-artifacts-"));
});

afterEach(() => {
  rmSync(root, { recursive: true, force: true });
});

function write(relPath: string, content: string): string {
  const absolute = join(root, relPath);
  mkdirSync(join(absolute, ".."), { recursive: true });
  writeFileSync(absolute, content, "utf8");
  return absolute;
}

// 把文件时间戳推到过去，模拟「本轮之前就存在的旧文件」。
function age(relPath: string, secondsAgo: number): void {
  const when = (Date.now() - secondsAgo * 1000) / 1000;
  utimesSync(join(root, relPath), when, when);
}

describe("artifact collection", () => {
  it("reports files created during the turn", () => {
    const before = snapshotWorkdir(root);
    write("report.html", "<html>hi</html>");
    write("data/table.csv", "a,b\n1,2\n");

    const produced = collectProducedArtifacts(root, before).map((item) => item.name).sort();
    expect(produced).toEqual(["report.html", "table.csv"]);
  });

  it("ignores files that were already there and untouched", () => {
    write("old.md", "# old");
    age("old.md", 600);
    const before = snapshotWorkdir(root);

    expect(collectProducedArtifacts(root, before)).toEqual([]);
  });

  it("picks up files modified during the turn", () => {
    write("notes.md", "# v1");
    age("notes.md", 600);
    const before = snapshotWorkdir(root);

    write("notes.md", "# v2 — rewritten this turn");
    const produced = collectProducedArtifacts(root, before);
    expect(produced.map((item) => item.name)).toEqual(["notes.md"]);
  });

  it("skips dependency and build directories entirely", () => {
    const before = snapshotWorkdir(root);
    write("node_modules/pkg/index.js", "module.exports = {}");
    write("dist/bundle.js", "code");
    write(".cache/thing.json", "{}");
    write("kept.md", "# kept");

    expect(collectProducedArtifacts(root, before).map((item) => item.name)).toEqual(["kept.md"]);
  });

  it("skips dotfiles, our own scratch files and uninteresting extensions", () => {
    const before = snapshotWorkdir(root);
    write(".env", "SECRET=1");
    write("cm-revise-card-1.html", "<html>scratch</html>");
    write("debug.log", "noise");
    write("image.png", "not-really-png");

    expect(collectProducedArtifacts(root, before).map((item) => item.name)).toEqual(["image.png"]);
  });

  it("drops empty and oversized files", () => {
    const before = snapshotWorkdir(root);
    write("empty.md", "");
    write("big.md", "x".repeat(2048));

    const produced = collectProducedArtifacts(root, before, { maxFileBytes: 1024 });
    expect(produced.map((item) => item.name)).toEqual([]);
  });

  it("caps the number of reported files, newest first", () => {
    const before = snapshotWorkdir(root);
    for (let index = 0; index < 5; index += 1) {
      write(`file-${index}.md`, `# ${index}`);
      // file-0 最旧（100 秒前），file-4 最新（60 秒前）。
      age(`file-${index}.md`, 100 - index * 10);
    }
    const produced = collectProducedArtifacts(root, before, { maxFiles: 2 });
    expect(produced.map((item) => item.name)).toEqual(["file-4.md", "file-3.md"]);
  });

  it("respects the recursion depth limit", () => {
    const before = snapshotWorkdir(root, { maxDepth: 1 });
    write("a/b/c/deep.md", "# deep");
    expect(collectProducedArtifacts(root, before, { maxDepth: 1 })).toEqual([]);
  });

  it("returns nothing when no working directory is configured", () => {
    expect(snapshotWorkdir(undefined).size).toBe(0);
    expect(collectProducedArtifacts(undefined, new Map())).toEqual([]);
  });
});

describe("previewable classification", () => {
  it("treats pages, docs and images as previewable", () => {
    expect(isPreviewableArtifact("index.html")).toBe(true);
    expect(isPreviewableArtifact("REPORT.PDF")).toBe(true);
    expect(isPreviewableArtifact("chart.svg")).toBe(true);
    expect(isPreviewableArtifact("data.csv")).toBe(true);
  });

  it("leaves archives and binaries to plain download", () => {
    expect(isPreviewableArtifact("bundle.zip")).toBe(false);
    expect(isPreviewableArtifact("app.exe")).toBe(false);
  });
});
