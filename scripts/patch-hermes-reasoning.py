#!/usr/bin/env python3
"""给本机 hermes 的 stream-json 协议补上思考（reasoning）事件。

背景
----
`hermes chat -q … --format stream-json` 是设备端拿「逐字正文 + 工具步骤」的唯一
通道，但它在静默模式（-q 必然静默）里会把 `agent.reasoning_callback` 置为
None（`cli._configure_quiet_agent`），而模型的思考 delta 只走这个回调 ——
于是思考整段被丢掉，JSONL 里只剩 text / tool_use / tool_result。

本脚本在 `hermes_cli/stream_json.py` 的 `StreamJsonEmitter.attach()` 里重新挂上
reasoning 回调，并输出 `{"type":"reasoning","text":…}` 增量事件。设备端 CLI
（`parseStreamJsonLine`）已经认识这个形状，无需其他改动。

用法
----
    python scripts/patch-hermes-reasoning.py            # 自动找安装目录
    python scripts/patch-hermes-reasoning.py --dir <hermes-agent 目录>
    python scripts/patch-hermes-reasoning.py --check    # 只检查，不改文件
    python scripts/patch-hermes-reasoning.py --revert   # 回滚到最近一次备份

幂等：已经打过补丁会直接跳过（退出码 0）。⚠️ hermes 升级会覆盖该文件，升级后
需重跑一次。
"""

from __future__ import annotations

import argparse
import os
import pathlib
import shutil
import sys
import time

MARKER = "on_reasoning_delta"

ATTACH_OLD = """        agent.stream_delta_callback = self.on_text_delta
        agent.tool_progress_callback = self.on_tool_progress
        return self

    def on_text_delta(self, text: str | None) -> None:"""

ATTACH_NEW = """        agent.stream_delta_callback = self.on_text_delta
        agent.tool_progress_callback = self.on_tool_progress
        # Reasoning deltas: in a TTY session they are painted into a [thinking] box by
        # ``reasoning_callback``; quiet one-shot runs null that callback out, so the model's
        # thinking was silently dropped from the protocol. Re-attach it here so structured
        # consumers can render the reasoning trace too. The event flow is delta-based.
        agent.reasoning_callback = self.on_reasoning_delta
        return self

    def on_reasoning_delta(self, text: str | None) -> None:
        if text:
            self._emit({"type": "reasoning", "text": str(text)})

    def on_text_delta(self, text: str | None) -> None:"""


def candidate_dirs() -> list[pathlib.Path]:
    home = pathlib.Path.home()
    out: list[pathlib.Path] = []
    env = os.environ.get("HERMES_AGENT_DIR")
    if env:
        out.append(pathlib.Path(env))
    local = os.environ.get("LOCALAPPDATA")
    if local:
        out.append(pathlib.Path(local) / "hermes" / "hermes-agent")
    out += [
        home / "AppData" / "Local" / "hermes" / "hermes-agent",
        home / ".local" / "share" / "hermes" / "hermes-agent",
        home / ".hermes" / "hermes-agent",
    ]
    return out


def locate(explicit: str | None) -> pathlib.Path | None:
    for base in ([pathlib.Path(explicit)] if explicit else candidate_dirs()):
        target = base / "hermes_cli" / "stream_json.py"
        if target.is_file():
            return target
    return None


def main() -> int:
    ap = argparse.ArgumentParser(description="patch hermes stream-json reasoning events")
    ap.add_argument("--dir", help="hermes-agent 安装目录（含 hermes_cli/stream_json.py）")
    ap.add_argument("--check", action="store_true", help="只检查是否已打补丁")
    ap.add_argument("--revert", action="store_true", help="回滚到最近一次备份")
    args = ap.parse_args()

    path = locate(args.dir)
    if path is None:
        print("找不到 hermes_cli/stream_json.py，请用 --dir 指定 hermes-agent 目录", file=sys.stderr)
        return 2

    source = path.read_text(encoding="utf-8")
    patched = MARKER in source

    if args.check:
        print(f"{path}: {'已打补丁' if patched else '未打补丁'}")
        return 0

    if args.revert:
        backups = sorted(path.parent.glob(path.name + ".bak-*"))
        if not backups:
            print("没有找到备份，未做任何改动", file=sys.stderr)
            return 2
        shutil.copy2(backups[-1], path)
        print(f"已回滚到 {backups[-1].name}")
        return 0

    if patched:
        print(f"{path} 已是补丁版本，跳过")
        return 0

    if ATTACH_OLD not in source:
        print(
            "stream_json.py 的结构与预期不符（hermes 版本可能变了），"
            "请手工在 StreamJsonEmitter.attach() 里挂 reasoning_callback",
            file=sys.stderr,
        )
        return 3

    backup = path.with_name(f"{path.name}.bak-{time.strftime('%Y%m%d%H%M%S')}")
    shutil.copy2(path, backup)
    updated = source.replace(ATTACH_OLD, ATTACH_NEW, 1)

    # 落盘前先编译校验：补丁坏了等于把用户的 hermes 弄挂。
    try:
        compile(updated, str(path), "exec")
    except SyntaxError as exc:  # noqa: TRY003
        print(f"补丁后语法校验失败，未写入：{exc}", file=sys.stderr)
        return 4

    path.write_text(updated, encoding="utf-8")
    print(f"已打补丁：{path}\n备份：{backup.name}")
    print("验证：hermes chat -q 'hi' --oneshot --format stream-json | grep '\"type\": \"reasoning\"'")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
