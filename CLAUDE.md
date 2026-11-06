# footnote

出處式知識問答機器人。使用者在 Discord（或本機測試頁）問開發問題，機器人從 repo 的 Markdown／xlsx 文件與 FAQ 卡裡找出答案並附出處；
FAQ 卡照原文回，其餘由 Claude 依檢索到的段落整理；找不到就明說，並把問題記成知識缺口。缺口由使用者啟動的代理（例如 Claude Code）
離線處理、寫成 FAQ 卡提案，人在 Discord 核准後機器人通知原提問者。

**設計說明：[docs/design.md](docs/design.md)**。寫卡規則在 `docs/faq-rules.md`，`/handle-gaps` 在 `.claude/commands/`。

- 範例語料在 `examples/`（虛構的「Acme 平台」），設定範本是 `footnote.example.yaml`；本機設定 `footnote.yaml` 不進版控。
- 缺口只存在 footnote 的 `gaps` 表，用 `footnote gaps` 指令操作。footnote 對 GitLab 只讀，不要加任何寫入 GitLab 的程式碼。

## 技術

Go 1.25+；Postgres 18 + pgvector 0.8；TEI（嵌入、重排序，自架）；Claude API（`internal/llm`，Anthropic Go SDK）用在整理回答與翻譯補查。
本機容器預設用 podman（Makefile 的 `COMPOSE`、`CONTAINER` 可換成 docker），TEI 跑 GPU（CPU 上重排太慢），啟動步驟見 README。
單一執行檔 `footnote`，模組切分見 `docs/design.md`。module path 是 `github.com/miskawu/footnote`。

## 設計約束（不要為了讓功能動起來而放寬）

- **線上回覆只有三種內容：FAQ 卡原文、文件原文摘錄、Claude 依檢索段落整理的回答。** 第三種只在閘門 A 通過且最高分不是 FAQ 卡時呼叫；
  回答要附出處，閘門 B（模型判斷沒有）與閘門 C（引用對不上）改成 no_data；API 錯誤、拒答或當月花費超過 `anthropic.monthly_budget_usd` 就退回原文摘錄。
- **翻譯補查只改查詢、不產生回覆內容**：閘門 A 沒過時模型把問題改寫成文件用語再查一次，救回的照樣經過閘門 B、C；用量記在 `asks.rescue_*`、
  跟整理回答共用月預算。改寫格式、給模型的參考資料改了要重跑 `footnote eval`，確認陷阱題沒被救成答案。
- **footnote 服務不得呼叫代理或 Claude Code 來即時回答。** 代理只做使用者啟動的離線工作。
- **寧可說沒有。** 閘門 A 判定不足就走 no_data。τ 只能拿 `footnote eval --sweep` 的數據改，改了要附 sweep 表。
- **缺口狀態只有一個來源：`gaps` 表。** 改狀態只有兩條路：`footnote gaps` 指令，以及 recheck 的自動判定。
  不要另外長出第二份狀態（例如以 GitLab issue 的開關為準）。
- **外部呼叫失敗記 `reason=error`，不記成知識缺口。**
- **FAQ 卡的核准只有一條路：`bot.reviewers` 列的人在 Discord 按。** 指令列不要加 approve，代理不能審自己的卡。
  正本是 `kb_changes`，現行內容由修改紀錄算出來，不要另外存一份「目前版本」。`footnote faq import` 只能在表是空的時候跑。
- **詞彙表是唯一能由代理核准的內容**：只限一次加一組叫法，要過 `footnote glossary accept` 裡的程式檢查，
  由 `glossary-reviewer` 子代理決定（`.claude/hooks/glossary-guard.py` 擋其他代理）。FAQ 卡原文直接回給使用者，不經過閘門 B、C，不走這條路。
- **FAQ 卡只能引用全員可讀專案的內容**；`faq lint --check-sources` 是這條的程式防線，不要放寬。
- `bot.channels` 是「哪裡能問、能搜哪些專案」的唯一一張表，沒列的頻道與私訊不回答。不要為了方便改回預設全開。
- SQL 集中在 `internal/store`。secret 只走環境變數，不進設定檔、不進版控。
- 各層透過介面（Embedder、Reranker、Handler 等）測試，單元測試用假實作，不打真的 TEI、GitLab。
- 測試資料、範例與文件只用虛構內容（例如 `acme/handbook`、`gitlab.example.com`），不放任何真實組織的文件或網址。

## 測試

commit 前跑這條（需要先 `make up` 與 `make migrate`，整合測試要連本機 Postgres）。CI（`.github/workflows/ci.yml`）跑的也是同一條：

```
make test
```

內容是 `go vet ./...` 加上 `go test ./...`，並帶 `FOOTNOTE_TEST_DATABASE_URL` 指向本機的 `footnote_test`
資料庫跑整合測試（會清空該資料庫）。沒設這個環境變數時整合測試會自動略過。
