#!/usr/bin/env python3
"""誰能跑哪些 footnote glossary 指令。

詞彙表是 FAQ 卡以外唯一能由代理核准的內容，前提是核准的代理不是提案的代理。這件事寫在
/handle-gaps 裡不算數：同一個代理想省事直接核准，流程文件擋不住它，所以由 hook 擋。

- accept：只有 glossary-reviewer 子代理能跑。主代理（整理候選的那一個）不能跑。
- revert：所有代理都不能跑。撤回是負責人的決定；他自己在終端機跑或按卡片，都不經過這個 hook。

agent_type 只在子代理裡才有，所以拿不到這個欄位時一律當成主代理，擋下來（寧可流程卡住看得到，
不要默默放行）。程式檢查在 footnote glossary accept 裡，這裡只管「誰」。
"""
import json
import re
import sys

REVIEWER = "glossary-reviewer"

# 指令開頭（可以有環境變數前綴、路徑），不比對字串裡提到的文字：commit 訊息、grep 不該被擋。
COMMAND = re.compile(
    r"""(?:^|[;&|(\n]|\$\(|-c\s+['"]|\s--\s+)\s*(?:\w+=\S*\s+)*(?:[\w./~$-]*/)?footnote\b[^;&|\n]*?\bglossary\s+(accept|revert)\b""",
    re.MULTILINE,
)


def main() -> int:
    try:
        event = json.load(sys.stdin)
    except json.JSONDecodeError:
        return 0
    command = (event.get("tool_input") or {}).get("command") or ""
    agent = event.get("agent_type") or ""
    for match in COMMAND.finditer(command):
        action = match.group(1)
        if action == "accept" and agent == REVIEWER:
            continue
        if action == "accept":
            print("footnote glossary accept 只有 glossary-reviewer 子代理能跑：核准的代理不能是提案的代理。", file=sys.stderr)
        else:
            print("footnote glossary revert 只有負責人能跑：把指令列在回報裡，由他決定。", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
