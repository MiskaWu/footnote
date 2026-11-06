---
description: 處理 footnote 的知識缺口與過期 FAQ 卡：分流、查證、寫卡、標記
argument-hint: "[缺口編號，例：12 15；不給就處理全部待處理的]"
---

處理 footnote 的知識缺口。先讀過 `docs/faq-rules.md` 的寫卡規則，整個過程都照那份規則做。

要處理的缺口：$ARGUMENTS（空白表示全部待處理的缺口，加上過期的卡）。

## 1. 確認環境、取清單

1. `footnote faq changes --status pending` 看還在待審的提案，已經有人提過的缺口不要重做；
   `footnote faq changes --status rejected` 看被退回的提案與原因，還沒改好的先處理。
   待審提案的 `review_via` 是 `agent` 時，是上一輪沒跑完的詞彙表提案：先照第 5 步第 3 點叫審核代理處理，再開始新的。
2. `footnote gaps --open --json` 取待處理的缺口。有指定編號時，只處理那幾個；指定的編號不在待處理清單裡，就用
   `footnote gaps --json` 查它現在的狀態並回報，不處理。
3. 沒指定編號時，再跑 `footnote faq stale --json` 取過期的卡，以及 `footnote glossary candidates --json` 取靠翻譯補查才答得出來的提問（第 5 步用）。
4. 環境出問題就停下來回報錯誤訊息（見 `docs/faq-rules.md`「環境」）。

缺口的 JSON 裡：`question` 是第一次的問法；`asks` 是每一次被問的問法（`question`）、檢索字串（`query`，討論串追問時
含同串的前一題，看它才知道「那可以找同事一起做嗎？」問的是哪個系統）與機器人當時的回覆；`kind` 是缺口怎麼來的
（`no_data` 沒找到、`unhelpful` 有回文件段落但被按 👎、`wrong_answer` 回了 FAQ 卡但被按 👎）；
`near_misses` 是當時最接近的段落（`project`、`path`、`heading` 組成 `project:path#heading` 就能給 `footnote show`）。

## 2. 每個缺口先分流，只走一條

先把 `asks` 裡的所有問法看過，弄清楚提問者到底想知道什麼。然後去找答案：

- `footnote search "<問法>"`，換幾種說法多搜幾次（文件用語常跟提問者不同，例：回滾／rollback、值班／on-call）。
  詞彙表有收的叫法，它會先印一行「檢索字串」，顯示補上了哪些叫法。
- `footnote grep "叫法"` 列出全部文件裡出現這個字的每一行，比對規則跟詞彙表相同。
- 對排名前幾的段落和 `near_misses` 用 `footnote show` 讀原文。只讀一個分頁不夠時，`footnote show "project:path"`
  印整份再 `grep -n` 關鍵字，看答案還寫在哪些分頁——同一份文件的不同分頁常常說法不同。

依找到的結果分流：

**A. 一般技術問題**（不是公司特有的知識，例如程式語言、工具、通用的軟體概念）
⇒ `footnote gaps dismiss <id>`，不寫卡。

**B. 讀得到答案**（文件有寫只是沒被找到、答案藏在文字方塊或長表格裡，或要對照幾個分頁才答得完整）
⇒ 寫卡，`gap` 填這個缺口的編號。`questions` 放提問者的問法，再加上文件用語的問法。
每個事實都要有讀過的來源；某一句找不到來源就拿掉那一句，整張卡撐不起來就改走 C。
文件之間互相矛盾時照寫卡規則 8 寫，並在回報的「需要人判斷」列出。

**C. 查不到**（文件沒有，知識只在某人腦中；或答案只寫在待討論、草稿之類不是定案的地方，見規則 9）
⇒ `footnote gaps needs-human <id> --note "…"`，note 寫三件事：查過哪些檔案與分頁、缺的是什麼、可能要問誰
（寫職責，例：負責憑證管理的維運；文件裡有負責人名字的可以附上）。不要猜，不要寫卡。

**D. `wrong_answer`**（回了 FAQ 卡但被說錯）
⇒ 從 `asks` 的回覆找出是哪張卡，用 `footnote faq cat` 看卡片、用 `footnote show` 重讀它的每個來源：內容錯了就修正
（`verified` 改成今天），來源已經不支持這個答案就用 `footnote faq retire` 提案刪卡。

**E. `unhelpful`**（回了文件段落但沒幫到）
⇒ 看 `asks` 裡機器人當時回了哪幾段，判斷是 A、B、C 還是 F，照那一條做。
`query` 有括號補上的叫法、而且就是補錯才答歪的，先用 `footnote faq changes --status approved` 找出加這組的提案，列進回報的「可能要撤回」，並附 `footnote glossary revert <編號>`。

**F. 只是叫法不同**（提問者用的是別名、縮寫或英文，例如文件寫「回滾」、他問「rollback」；把文件的叫法補在別名後面再搜，
第一名就是答案所在的分頁、且標「通過閘門 A」）
⇒ 記下這個叫法，第 5 步提案；不寫卡。規則見 `docs/faq-rules.md`「詞彙表」。
叫法可能指兩件事、或補上之後還是排不到，就不是這條：照 B 寫卡（`questions` 放提問者的說法）或照 C 交給人。
`asks` 的 `query` 已經有括號補上的叫法時，代表詞彙表當時就補了還是沒找到，也不是這條。

## 3. 過期的卡

`faq stale` 列出的每張卡，對照列出的變動來源，用 `footnote show` 重讀：

- 內容仍正確 ⇒ 只把 `verified` 改成今天。
- 有變 ⇒ 依新內容改本文與 sources，`verified` 改成今天。
- 已經不適用（功能拿掉、整份文件刪了）⇒ 用 `footnote faq retire` 提案刪卡。

## 4. 寫卡與提案

每張卡（含第 2 步 D 與第 3 步的修改）：

1. 修改既有的卡時，先 `footnote faq cat <路徑>` 取現行內容；新卡寫在暫存檔。
2. 寫卡前，每個打算引用的來源都先用 `footnote show "project_path:path#分頁"` 讀過；分頁名稱以它印出來的為準。
3. 寫完跑 `footnote faq lint --check-sources <卡片路徑>`，不過就修到過。
4. 照 `docs/faq-rules.md`「送審」提案，一張卡一個提案。**你不能核准自己的提案。**

寫了卡的缺口**不用改狀態**：人在 Discord 核准後，footnote 馬上同步，看到卡片的 `gap` 就會把缺口關掉，並通知當初的提問者。

## 5. 詞彙表：你整理，另一個代理決定

候選有兩個來源：第 2 步分流 F 的缺口，以及 `footnote glossary candidates` 的提問（同事的原話對照補查改寫的結果，看得出哪個詞被換掉）。
先用 `footnote faq changes --status rejected` 看被退回過的叫法：沒有新證據就不要重提，退回原因寫「交給人：」的列進回報。

**一次一組、做完一組才做下一組**，一輪最多 5 組。詞彙表只有一份，同時只能有一個待審提案。每一組：

1. `footnote faq cat glossary.yaml` 取現行內容，加一行（新的一組第一個寫文件裡的正式名稱），或在既有那組加上這個叫法。上面用註解寫來源。
   存成檔名 `glossary.yaml` 的暫存檔，`footnote faq lint <暫存檔>` 確認通過。
2. `footnote glossary propose --file <暫存檔> --note "叫法與來源編號"`，例：`--note "cert：提問 #50"`、`--note "rollback：G12"`。
   **note 只寫這些**：叫法與來源編號，不寫你為什麼覺得對。審核要獨立判斷，看到你的理由就只是在確認你。
3. 用 Agent 工具叫 `glossary-reviewer`，prompt 只有一句：「審核詞彙表提案 #<編號>」。**不要附你查到的東西、你的判斷或第 2 步的思路。**
4. 等它回覆再做下一組。它核准之後馬上生效，同步會重跑 `no_data` 的缺口，通過閘門 A 就標成可能已解決並通知提問者。`unhelpful` 的缺口不會自動重跑，
   在回報的「需要人判斷」寫明要用 `footnote gaps close <id>` 確認。

**你不能核准自己的提案**：`footnote glossary accept` 只有 `glossary-reviewer` 能跑，hook 會擋你。被退回就接受，要改寫得有新證據，
重提時同樣走一次審核。要拿掉或改掉線上的叫法不走這條：用 `footnote faq propose glossary.yaml` 提案，給人審。

## 6. 回報

最後用一張表回報每個缺口與過期卡：編號、問題、分流（A–F 或過期）、做了什麼（卡片路徑與提案編號／詞彙表加了哪組與提案編號／dismiss／needs-human 的 note）。
表後面列出等人在 Discord 審的提案編號。

**詞彙表另外列一份清單**給負責人（資料來自 `footnote faq changes --status approved` 與 `--status rejected`）：

- 核准的：叫法 ⇒ 文件叫法、提問原話、線上分數 ⇒ 提案分數、陷阱題分數，以及撤回指令 `footnote glossary revert <編號>`。撤回只有負責人能跑，把指令寫出來就好。
- 退回的：叫法與原因。
- 交給人的：叫法與要問什麼。

再另外列出「需要人判斷」：矛盾的卡、拿不準的分流，以及照文件跑卻出錯、要更新文件的指令。
