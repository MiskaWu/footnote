#!/bin/sh
# 測 glossary-guard.py：sh .claude/hooks/glossary-guard.test.sh
cd "$(dirname "$0")" || exit 1
fail=0
check() { # 期望的結束碼、agent_type、指令
  got=$(python3 -c 'import json,sys; print(json.dumps({"tool_input":{"command":sys.argv[2]},"agent_type":sys.argv[1]}))' "$2" "$3" | python3 glossary-guard.py 2>/dev/null; echo $?)
  if [ "$got" != "$1" ]; then echo "✗ 期望 $1 得到 $got：[$2] $3"; fail=1; fi
}
check 2 ""                  'footnote glossary accept 12 --note x'
check 2 ""                  './bin/footnote -c a.yaml glossary accept 12'
check 2 ""                  'FOOTNOTE_DATABASE_URL=postgres://x bin/footnote glossary accept 12'
check 2 ""                  'cd ~/projects/footnote && footnote glossary accept 12'
check 2 ""                  "bash -c 'footnote glossary accept 12'"
check 2 ""                  'kubectl exec -i deploy/footnote -- footnote glossary accept 12'
check 2 "general-purpose"   'footnote glossary accept 12'
check 0 "glossary-reviewer" 'footnote glossary accept 12 --ask "a=>b#c" --trap d --note e'
check 2 "glossary-reviewer" 'footnote glossary revert 12'
check 2 ""                  'footnote glossary revert 12'
check 0 ""                  'footnote glossary check 12'
check 0 ""                  'footnote glossary propose --file x --note y'
check 0 ""                  'footnote glossary candidates --json'
check 0 ""                  'git commit -m "feat(kb): 加 footnote glossary accept 指令"'
check 0 ""                  'grep -n "footnote glossary accept" docs/plan.md'
[ "$fail" = 0 ] && echo "glossary-guard：全部通過"
exit $fail
