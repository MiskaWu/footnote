# footnote

出處式知識問答機器人。使用者在 Discord（或本機測試頁）問開發問題，機器人從 GitLab repo 的
Markdown／xlsx 文件與 FAQ repo 的 FAQ 卡裡找出答案並附出處，**線上不呼叫任何 LLM**；
找不到就明說，並把問題記成知識缺口。缺口由使用者啟動的代理（例如 Claude Code）
離線處理、寫成 FAQ 卡開 MR，人審合併後機器人通知原提問者。

**完整計畫：[docs/plan.md](docs/plan.md)**。照里程碑 M0→M6 的順序做，動手前先讀該里程碑
的段落與驗收條件；做完一個里程碑要能逐條對上驗收。

## 現況

- M0–M4 在本機可用，另外做了 FAQ 卡的讀取、lint 與過期偵測；現況與偏離計畫之處見 plan §15。
  v1 線上不呼叫 LLM（成本考量），即時生成設計在 plan 附錄 A。
- 範例語料在 `examples/`（虛構的「Acme 平台」），設定範本是 `footnote.example.yaml`；
  本機設定 `footnote.yaml` 不進版控。
- 提問入口是 Discord（plan §7.8），程式完成但尚未實測；本機測試頁可代替。
- **v1 不做 GitLab issue**：缺口只存在 footnote 的 `gaps` 表，用 `footnote gaps` 指令操作；
  代理指令放 FAQ repo 的 `.claude/`（plan D6）。footnote 對 GitLab 只讀，不要加任何寫入 GitLab 的程式碼。
  issue 同步是 plan §12 第 6 項，使用者說要做才做。

## 技術

Go 1.25+；Postgres 16 + pgvector ≥ 0.8；TEI（嵌入、重排序，自架）。v1 不用任何 LLM API。
本機容器預設用 podman（Makefile 的 `COMPOSE`／`CONTAINER` 可換成 docker），TEI 跑 GPU
（CPU 上重排太慢，實測見 plan §15），啟動步驟見 README。
單一執行檔 `footnote`，子命令見 plan §3，模組切分見 plan §6。module path 是 `github.com/miskawu/footnote`。

## 設計約束（不要為了讓功能動起來而放寬）

- **線上回覆只有兩種內容：FAQ 卡原文、文件原文摘錄。** 不要為了「回答更好」加上生成、改寫或摘要；
  那是 v2（plan 附錄 A），要使用者拍板才做。
- **footnote 服務不得呼叫代理或 LLM 來即時回答**（plan D9）。代理只做使用者啟動的離線工作。
- **寧可說沒有。** 閘門 A 判定不足就走 no_data。τ 只能拿 `footnote eval --sweep` 的數據改，改了要附 sweep 表。
- **缺口狀態只有一個來源：`gaps` 表。** 改狀態只有兩條路：`footnote gaps` 指令，以及 recheck 的自動判定（plan §7.5）。
  不要另外長出第二份狀態（例如以 GitLab issue 的開關為準）。
- **外部呼叫失敗記 `reason=error`，不記成知識缺口。**
- **FAQ 卡只能引用全員可讀專案的內容**；`faq lint --check-sources` 是這條的程式防線，不要放寬。
- SQL 集中在 `internal/store`。secret 只走環境變數，不進設定檔、不進版控。
- 各層透過 plan §6 的介面（Embedder、Reranker、Adapter、Handler）測試，
  單元測試用假實作，不打真的 TEI、GitLab。
- 測試資料、範例與文件只用虛構內容（例如 `acme/handbook`、`gitlab.example.com`），不放任何真實組織的文件或網址。

## 測試

目前沒有 CI 設定，commit 前跑這條（需要先 `make up` 與 `make migrate`，整合測試要連本機 Postgres）：

```
make test
```

內容是 `go vet ./...` 加上 `go test ./...`，並帶 `FOOTNOTE_TEST_DATABASE_URL` 指向本機的 `footnote_test`
資料庫跑 `internal/gap`、`internal/ingest`、`internal/store` 的整合測試（會清空該資料庫）。
沒設這個環境變數時整合測試會自動略過。
