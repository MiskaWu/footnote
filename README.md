# footnote

> **footnote** is a citation-first Q&A bot for a team's internal docs. It answers only with verbatim
> excerpts from your GitLab repos (Markdown and xlsx) or with human-reviewed FAQ cards, always with a
> link to the source — **no LLM is called at answer time**. Retrieval is hybrid (pgvector + full-text,
> merged with RRF) and reranked by a self-hosted cross-encoder (TEI); if the top score does not clear a
> calibrated threshold, footnote says it has nothing and records the question as a *knowledge gap*.
> Gaps are deduplicated, turned into FAQ cards offline by an agent, merged through a reviewed MR, and the
> original askers are notified automatically. Written in Go; Discord bot plus a local test page.

footnote 是給團隊內部文件用的「出處式」知識問答機器人：從 GitLab repo 的文件（Markdown、xlsx 試算表）與人審過的 FAQ 卡裡找答案，
每則回覆都附出處連結，**線上不經任何 LLM 生成**。文件裡沒有的就明說「沒有」，同時把問題記成**知識缺口**；
缺口由離線代理查證、寫成 FAQ 卡走 MR 人審，合併後機器人自動回頭通知當初發問的人。

完整的設計脈絡、決策與實測紀錄在 [docs/plan.md](docs/plan.md)。

## 設計立場

- **線上零 LLM。** 機器人回的每個字，不是人審過的 FAQ 卡原文，就是文件的原文摘錄。不改寫、不摘要、不生成，
  所以「不編造」在結構上成立，每題的線上成本也是零。生成只發生在離線、由人啟動、經 MR 審核的流程裡。
- **查不到就說沒有。** 重排序後的最高分沒有通過閘門（`τ_answer`）就回「目前內部文件沒有相關資料」，
  不拿勉強相關的段落充數。閾值不靠感覺，靠評估集 `footnote eval --sweep` 掃出來。
- **知識缺口閉環。** 查不到、被按 👎 的問題都記成缺口，相似問法合併計數（`τ_dup`）。文件或 FAQ 卡更新後自動重檢，
  解決了就回到原討論串通知提問者。缺口狀態只有一個來源：資料庫的 `gaps` 表。
- **FAQ 卡也要有出處。** 每句事實標 `[n]` 對應來源檔案；引用的檔案一變動，卡片自動標成過期、回覆附警示，等人重新確認。

## 架構

```
離線：同步與索引（serve 內每 10 分鐘；或手動 footnote ingest）
  來源 checkout ─► 比對 blob sha ─► 抽文字（Markdown／xlsx）─► 密鑰掃描 ─► 切塊
    ─► TEI /embed ─► Postgres（pgvector + tsvector）─► FAQ 過期偵測 ─► 重檢缺口
       （advisory lock：同一時間只有一個同步）

線上：回答一個問題（不呼叫任何 LLM）
  Discord／本機測試頁 ─►（同一討論串的追問：前一問＋本問串接）
    ─► 混合檢索：向量 top 40 + 全文 top 40，RRF 合併取 30 ─► TEI /rerank
    ─► 閘門 A：最高分 < τ_answer ？
         是 ─► 回「沒有資料」─► 缺口去重 ─► 記進 gaps
         否 ─► 最高分是 FAQ 卡 ⇒ 回卡片原文＋出處
               否則 ⇒ 列前 k 個文件段落（重排序挑出最相關的幾行當摘錄）＋連結
    ─► 👎 ─► 缺口去重 ─► 記進 gaps

離線：處理缺口（使用者自己啟動代理，例如 Claude Code）
  footnote gaps --open --json ─► 查 repo ─► 寫 FAQ 卡 ─► footnote faq lint --check-sources
    ─► 開 MR ─► 人審合併 ─► 同步 ─► 重檢 ─► 通知寫進 outbox（notices 表）─► 回原討論串通知
```

技術：Go 1.25+、Postgres 16 + pgvector 0.8、[TEI](https://github.com/huggingface/text-embeddings-inference)
跑 `BAAI/bge-m3`（嵌入）與 `BAAI/bge-reranker-v2-m3`（重排序）。全文檢索的中文切詞是純 Go 的 bigram，不需要 cgo。
單一執行檔 `footnote`，模組切分見 plan §6。

## 快速開始

需要：Go 1.25+、podman（或 docker）compose、`curl`。TEI 預設跑 GPU：

- `deploy/compose.yaml` 是 **WSL2 + NVIDIA 顯卡**的設定（掛 `/dev/dxg`，不需要 NVIDIA Container Toolkit）；
  映像用 `turing-1.8`（RTX 20 系列），30／40 系列請改用對應的 tag。原生 Linux 的改法寫在該檔開頭的註解。
- 沒有 GPU 時用 `deploy/compose.cpu.yaml`，流程可以跑通，但重排一題要一分鐘以上，只適合確認流程。
- 第一次啟動 TEI 會從 Hugging Face 下載兩個模型，合計約 5GB。

```sh
make up        # Postgres + 兩個 TEI；沒有 GPU：make up COMPOSE_FILE=deploy/compose.cpu.yaml
make wait      # 等資料庫就緒、模型下載並載入完成
make ingest    # 建表，並把 examples/ 的範例語料同步進索引
make serve     # 開 http://127.0.0.1:28090 的本機測試頁
```

用 docker 的話在每個 make 指令後面加 `COMPOSE="docker compose -f deploy/compose.yaml" CONTAINER=docker`。
`make` 發現沒有 `footnote.yaml` 時會從 `footnote.example.yaml` 複製一份。

[`examples/`](examples/) 是一個虛構軟體平台「Acme 平台」的維運手冊（發版、回滾、備份、憑證、值班）和兩張 FAQ 卡。可以試問：

- 「正式環境要怎麼回滾到上一版？」→ 命中 FAQ 卡，回卡片原文與出處
- 「TLS 憑證外洩要怎麼換？」→ 列出文件段落與摘錄
- 「API 頻率限制是多少？」→ 命中 `drafts/` 底下的提案，回覆標「草稿」
- 「員工旅遊補助要怎麼申請？」→ 回「沒有資料」，右側的知識缺口清單多一筆

範例的出處連結指向 `gitlab.example.com`，點了不會通。也可以不開網頁，直接在命令列試：

```sh
./bin/footnote ask "正式環境要怎麼回滾到上一版？"   # 印出會回的內容（不記缺口）
./bin/footnote search "資料庫快照保留幾天"          # 只看檢索排名與分數
```

## 接上自己的文件

1. `cp footnote.example.yaml footnote.yaml`（已在 `.gitignore`，不會進版控）。
2. 改 `gitlab_url`，`sources` 每個知識來源一筆：`project_path` 是 GitLab 上的專案路徑（組連結用），
   `dir` 是本機的 checkout。`dir` 是 git repo 根目錄時，讀 `ref` 已 commit 的內容（LFS 檔會還原）。
   `include`／`exclude` 用 glob，支援 `.md`、`.mdx`、`.xlsx`。
3. `faq` 指向放 FAQ 卡的 repo。卡片格式見 [`examples/faq/`](examples/faq/) 與 plan §7.6。
4. 換了語料就要重新校準 `τ_answer`：照 [`eval/eval.example.yaml`](eval/eval.example.yaml) 的格式寫評估集，
   `make eval EVAL=你的評估集.yaml`。
5. 要接 Discord：把 bot token 放進環境變數 `DISCORD_BOT_TOKEN`（`bot.token_env` 可改名）再 `make serve`。
   secret 只走環境變數，不寫進設定檔。

其他指令見 `./bin/footnote help`：缺口管理是 `footnote gaps`（`dismiss`／`close`／`needs-human`），
FAQ 卡檢查是 `footnote faq lint --check-sources`、`footnote faq stale`。

## 評估

`footnote eval -f <評估集> --sweep` 對每題只跑一次檢索與重排序，存下分數後在本機把 `τ_answer` 從 0.05 掃到 0.80，
印出每個閾值的**誤導率**（該拒答卻通過閘門的比例）與**誤拒率**（該回答卻被擋下的比例），
並建議「誤導率達標下誤拒率最低」那段區間的中間值。

作者在私有文件集（74 個 xlsx 試算表，同步後 1,127 個片段）上的實測：

- 評估集 28 題（應答 14、拒答 10、一般技術問題 4，出處寫到分頁）：**Recall@3 = 1.00，MRR = 0.71**。
- 拒答題的最高分是 **0.151**，應答題的最低分是 **0.651**，中間沒有任何題目；
  `τ_answer` 在 0.20–0.65 之間都是零誤導、零誤拒，取中間值 **0.40**。
- 重排序 30 個候選：GPU（RTX 2080 Ti）約 **1 秒**，CPU **60 秒以上**。
- 全量同步：GPU 約 **1.5 分鐘**，CPU 約 **50 分鐘**（推估）。一題端到端約 1.5–1.7 秒。

數字來自作者在私有文件集上的實測，評估集未公開；`eval/eval.example.yaml` 只示範格式。
題數還少、拒答題偏簡單，數字偏樂觀，細節與已知限制見 plan §15。

## 目前狀態

| 範圍 | 狀態 |
|---|---|
| 同步、切塊（Markdown／xlsx）、混合檢索、重排、閘門 A、回覆組裝 | 本機可用（M0–M2） |
| 缺口去重、`footnote gaps` 指令、重檢、通知（outbox） | 本機可用，有整合測試（M4） |
| FAQ 卡解析、lint、`--check-sources`、過期偵測 | 本機可用，有整合測試 |
| 本機測試頁 | 可用，可代替 Discord 測試 |
| Discord bot | 程式完成、有單元測試，**尚未實測** |
| GitLab API 來源、push webhook | **未做**，目前只讀本機 checkout |
| 處理缺口的代理指令（M5）、k8s 部署與 `/metrics`（M6） | 未做 |

## 開發

```sh
make test   # go vet + go test；整合測試連 FOOTNOTE_TEST_DATABASE_URL（會清空該資料庫）
```

整合測試需要一個可以清空的 Postgres + pgvector；`make migrate` 會在 compose 的資料庫裡建好 `footnote_test`。
沒設 `FOOTNOTE_TEST_DATABASE_URL` 時整合測試自動略過。單元測試一律用假的嵌入與重排序，不打真的 TEI。

## 授權

[MIT](LICENSE)
