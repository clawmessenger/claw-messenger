import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

export interface EchoAgentFixture {
  dir: string;
  agentPath: string;
  recordedArgs: () => string[];
  cleanup: () => void;
}

export interface EchoAgentOptions {
  recordArgs?: boolean;
  hangMs?: number;
  bigOutputBytes?: number;
  failWith?: { code: number; message: string };
}

// Creates a temporary directory holding a fake agent executable named `name`
// (claude.cmd on Windows, claude elsewhere) that echoes stdin back, can record
// its argv to a file, hang, emit a huge payload, or exit with a failure.
export function createEchoAgentScript(name: string, opts: EchoAgentOptions = {}): EchoAgentFixture {
  const dir = mkdtempXiachat();
  const isWin = process.platform === "win32";
  const scriptPath = join(dir, isWin ? `${name}.cmd` : name);
  const argsFile = join(dir, "args.txt");

  if (isWin) {
    const lines = ["@echo off"];
    if (opts.recordArgs) lines.push(`echo %*> "${argsFile}"`);
    if (opts.failWith) {
      lines.push(`echo ${opts.failWith.message} 1>&2`);
      lines.push(`exit /b ${opts.failWith.code}`);
    }
    if (opts.hangMs) lines.push(`ping -n ${Math.ceil(opts.hangMs / 1000) + 1} 127.0.0.1 >nul`);
    if (opts.bigOutputBytes) {
      lines.push(`powershell -NoProfile -Command "[Console]::Out.Write(([string]::new([char]97, ${opts.bigOutputBytes})))"`);
    } else if (!opts.failWith) {
      lines.push(`powershell -NoProfile -Command "$input | Select-Object -First 1"`);
    }
    lines.push("exit /b 0");
    writeFileSync(scriptPath, lines.join("\r\n") + "\r\n", { mode: 0o755 });
  } else {
    const lines = ["#!/bin/sh"];
    if (opts.recordArgs) lines.push(`echo "$@" > "${argsFile}"`);
    if (opts.failWith) {
      lines.push(`echo ${opts.failWith.message} >&2`);
      lines.push(`exit ${opts.failWith.code}`);
    }
    if (opts.hangMs) lines.push(`sleep $((${opts.hangMs} / 1000 + 1))`);
    if (opts.bigOutputBytes) {
      lines.push(`python3 -c "import sys; sys.stdout.write('a' * ${opts.bigOutputBytes})"`);
    } else if (!opts.failWith) {
      lines.push("cat");
    }
    lines.push("exit 0");
    writeFileSync(scriptPath, lines.join("\n") + "\n", { mode: 0o755 });
    chmodSync(scriptPath, 0o755);
  }

  return {
    dir,
    agentPath: scriptPath,
    recordedArgs: (): string[] => {
      if (!existsSync(argsFile)) return [];
      return readFileSync(argsFile, "utf8").split(/\s+/).filter(Boolean);
    },
    cleanup: () => rmSync(dir, { recursive: true, force: true }),
  };
}

function mkdtempXiachat(): string {
  const dir = join(tmpdir(), `xiachat-agent-${Date.now()}-${Math.random().toString(36).slice(2)}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}
