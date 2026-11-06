# footnote

> **footnote** is a citation-first Q&A bot for a team's internal docs. It searches your repos (Markdown and
> xlsx) and human-reviewed FAQ cards, then answers with a link to every source. Retrieval is hybrid
> (pgvector + full-text, merged with RRF) and reranked by a self-hosted cross-encoder (TEI). If the top score
> does not clear a calibrated threshold, footnote says it has nothing and records a *knowledge gap* instead of
> guessing. FAQ cards are returned verbatim; other answers are written by Claude from the retrieved passages
> only, and two more gates turn an unsupported answer back into "no data". Gaps are deduplicated, turned into
> FAQ cards offline by an agent, approved by a human in Discord, and the original askers are notified.
> Written in Go; Discord bot plus a local test page.

footnote 是給團隊內部文件用的「出處式」知識問答機器人：從 repo 的文件（Markdown、xlsx 試算表）與 FAQ 卡裡找答案，
每則回覆都附出處連結。文件裡沒有的就明說「沒有」，同時把問題記成**知識缺口**；缺口由離線代理查證、寫成 FAQ 卡提案，
人在 Discord 核准後，機器人自動回頭通知當初發問的人。

完整的設計脈絡見 [docs/design.md](docs/design.md)。

## 設計立場

- **線上回覆只有三種內容：** FAQ 卡原文、文件原文摘錄、Claude 依檢索到的段落整理的回答。第三種要附出處，
  模型判斷文件沒有答案（閘門 B）或引用對不上（閘門 C）就改回「沒有資料」；API 出錯或當月花費超過預算就退回原文摘錄。
- **FAQ 卡不經過模型。** 卡片是人審過的內容，照原文回，不改寫。
- **查不到就說沒有。** 重排序後的最高分沒有通過閘門 A（`τ_answer`）就回沒有資料，不拿勉強相關的段落充數。
  閾值不靠感覺，靠評估集 `footnote eval --sweep` 掃出來。
- **知識缺口閉環。** 查不到、被按 👎 的問題都記成缺口，相似問法合併計數（`τ_dup`）。文件或 FAQ 卡更新後自動重檢，
  解決了就回到原討論串通知提問者。缺口狀態只有一個來源：資料庫的 `gaps` 表。
- **FAQ 卡也要有出處。** 每句事實標 `[n]` 對應來源檔案；引用的檔案一變動，卡片自動標成過期，回覆時附警示。
- **代理不審自己的東西。** FAQ 卡只能由人在 Discord 核准，指令列沒有這條路。詞彙表例外，由另一個審核代理決定，
  但一次只能加一組，要過程式檢查，人隨時能一行撤回。
- **服務本身不呼叫代理。** 代理只做使用者啟動的離線工作，線上只用固定用途的 API 呼叫。

## 架構

```
離線：同步與索引（serve 內每 10 分鐘；或手動 footnote ingest）
  來源 checkout ─► 比對 blob sha ─► 抽文字（Markdown／xlsx）─► 密鑰掃描 ─► 切塊
    ─► TEI /embed ─► Postgres（pgvector + tsvector）─► FAQ 過期偵測 ─► 重檢缺口

線上：回答一個問題
  Discord／本機測試頁 ─►（同一討論串的追問：前一問＋本問串接）─► 依詞彙表補上文件的正式名稱
    ─► 混合檢索：向量 top 40 + 全文 top 40，RRF 合併取 30 ─► TEI /rerank
    ─► 閘門 A：最高分 < τ_answer ？
         是 ─► 翻譯補查：模型把外文、口語換成文件用語再查一次
              ─► 仍沒過 ─► 回「沒有資料」─► 缺口去重 ─► 記進 gaps
         否 ─► 最高分是 FAQ 卡 ⇒ 回卡片原文＋出處
               否則 ⇒ Claude 依段落整理（閘門 B、C）；沒有 API key 或超過預算時回原文摘錄
    ─► 👎 ─► 缺口去重 ─► 記進 gaps

離線：處理缺口（使用者自己啟動代理，例如 Claude Code 的 /handle-gaps）
  footnote gaps --open --json ─► 查文件 ─► 寫 FAQ 卡 ─► footnote faq propose
    ─► 人在 Discord 核准 ─► 同步 ─► 重檢 ─► 通知寫進 outbox（notices 表）─► 回原討論串通知
```

技術：Go 1.25+、Postgres 18 + pgvector 0.8、[TEI](https://github.com/huggingface/text-embeddings-inference)
跑 `BAAI/bge-m3`（嵌入）與 `BAAI/bge-reranker-v2-m3`（重排序）、Claude API（整理回答與翻譯補查）。
全文檢索的中文切詞是純 Go 的 bigram，不需要 cgo。單一執行檔 `footnote`，模組切分見 [docs/design.md](docs/design.md)。

## 快速開始

需要：Go 1.25+、podman（或 docker）compose、`curl`。TEI 預設跑 GPU：

- `deploy/compose.yaml` 是 **WSL2 + NVIDIA 顯卡**的設定（掛 `/dev/dxg`，不需要 NVIDIA Container Toolkit）；
  映像用 `turing-1.8`（RTX 20 系列），30 系列以上請改用對應的 tag。
- 沒有 GPU 時用 `deploy/compose.cpu.yaml`，流程可以跑通，但重排一題要一分鐘以上，只適合確認流程。
- 第一次啟動 TEI 會從 Hugging Face 下載兩個模型，合計約 5GB。

```sh
make up               # Postgres + 兩個 TEI；沒有 GPU：make up COMPOSE_FILE=deploy/compose.cpu.yaml
make wait             # 等資料庫就緒、模型下載並載入完成
make migrate          # 建表，並建立整合測試用的 footnote_test 資料庫
make import-examples  # 把範例 FAQ 卡與詞彙表寫進資料庫，只能做一次
make ingest           # 把 examples/ 的範例語料同步進索引
make serve            # 開 http://127.0.0.1:28090 的本機測試頁
```

用 docker 的話在每個 make 指令後面加 `COMPOSE="docker compose -f deploy/compose.yaml" CONTAINER=docker`。
`make` 發現沒有 `footnote.yaml` 時會從 `footnote.example.yaml` 複製一份。

[`examples/`](examples/) 是一個虛構軟體平台「Acme 平台」的維運手冊（發版、回滾、備份、憑證、值班）、兩張 FAQ 卡和一份詞彙表。可以試問：

- 「正式環境要怎麼回滾到上一版？」：命中 FAQ 卡，回卡片原文與出處。
- 「TLS 憑證外洩要怎麼換？」：沒設 API key 時列出文件段落與摘錄，設了就由 Claude 整理。
- 「API 頻率限制是多少？」：命中 `drafts/` 底下的提案，回覆標「未確認設計」。
- 「員工旅遊補助要怎麼申請？」：回「沒有資料」，右側的知識缺口清單多一筆。

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
3. FAQ 卡與詞彙表存在資料庫。既有內容用 `footnote faq import <目錄>` 寫進去，只能做一次；之後走提案。
   格式見 [`examples/faq/`](examples/faq/) 與 [docs/faq-rules.md](docs/faq-rules.md)。
4. 換了語料就要重新校準 `τ_answer` 與 `τ_follow`：照 [`eval/eval.example.yaml`](eval/eval.example.yaml) 的格式寫評估集，
   `make eval EVAL=你的評估集.yaml`。

### 接上 Discord

1. [Discord Developer Portal](https://discord.com/developers/applications) 建一個 Application，在 Bot 頁按 Reset Token 取得 token。
   Privileged Gateway Intents 都不用開（只靠 @ 與私訊觸發）。
2. OAuth2 → URL Generator：Scopes 勾 `bot`；Bot Permissions 勾 View Channels、Send Messages、Send Messages in Threads、
   Create Public Threads、Read Message History。用產生的網址把機器人加進伺服器。
3. token 寫進 `~/.config/footnote/secrets.env`（權限 600，不進版控），一行 `DISCORD_BOT_TOKEN=…`。`make serve` 會讀這個檔。
4. 在 `footnote.yaml` 的 `bot.channels` 列出開放的頻道。沒列的頻道與私訊不回答。
5. 在頻道裡 `@footnote 問題`，或私訊機器人。

### 接上 Claude（整理回答）

在 Anthropic Console 建 API key（建議同時設每月花費上限），加到同一個 `secrets.env`：一行 `ANTHROPIC_API_KEY=…`。
沒設的話機器人回原文摘錄。模型、預算在 `footnote.yaml` 的 `anthropic` 段；換模型前可以用
`footnote compare -f eval/compare.example.yaml` 讓幾個模型用同一份檢索結果各答一次，比較品質與花費。

### 常駐

```sh
make install-service   # 裝成 user 服務：footnote-stack（容器）與 footnote（機器人），當掉 10 秒後自動重啟
make restart           # 之後改完程式：重新 build 再重啟機器人
journalctl --user -u footnote -f   # 看 log
```

服務檔假設 repo 在 `~/projects/footnote`，放在別處要改 `deploy/systemd/`。
裝成服務後就不要再手動 `make serve`，不然兩個機器人會同時連上 Discord，每題回兩次。

## FAQ 卡與審核

FAQ 卡與詞彙表存在資料庫（`kb_changes`），每次修改都留紀錄。處理缺口：在這個 repo 開 Claude Code，跑 `/handle-gaps`（或 `/handle-gaps 12` 只處理 G12）。
寫卡規則在 [docs/faq-rules.md](docs/faq-rules.md)。代理用 `footnote faq propose` 提案，提案貼到 `bot.review_channel`，`bot.reviewers` 列的人按「核准上線」後馬上生效並通知提問者。
指令列沒有核准這條路，代理不能審自己的卡。

詞彙表是例外：`/handle-gaps` 裡代理一次提一組叫法（`footnote glossary propose`），由 `glossary-reviewer` 子代理讀文件後原樣核准或退回；
核准要過 `footnote glossary accept` 裡的程式檢查，馬上生效。每輪結束的清單列出核准的每一筆與證據，要撤回就跑 `footnote glossary revert <編號>`，只有人能跑。

其他指令見 `./bin/footnote help`：缺口管理是 `footnote gaps`，FAQ 卡檢查是 `footnote faq lint --check-sources`、`footnote faq stale`。

## 容器

`Dockerfile` 建的映像只有執行檔與 git、git-lfs，設定檔不在裡面。容器裡要的東西：

| 項目 | 怎麼給 |
|---|---|
| 設定檔 | 掛到 `/etc/footnote/footnote.yaml`（`FOOTNOTE_CONFIG` 可改）；`server.addr` 要 `0.0.0.0:28090` 健康檢查才打得到 `/healthz` |
| 文件 | 來源設 `dir: /data/…` 加 `clone: https://…git`，第一次同步時自動 clone 到 `/data`（掛持久磁碟） |
| 密鑰 | 環境變數：`FOOTNOTE_DATABASE_URL`、`DISCORD_BOT_TOKEN`、`ANTHROPIC_API_KEY`、`FOOTNOTE_GIT_USERNAME`／`FOOTNOTE_GIT_TOKEN`（`read_repository` 的 deploy token） |
| 審核 | `bot.review_channel` 是提案貼出來的頻道，`bot.reviewers` 是能按核准、退回的 Discord 使用者 id |
| 開放的頻道 | `bot.channels` 列出頻道 id；頻道 id 可以在那個頻道 @ 機器人一次，從 log 的 `question where not open` 看 |

```sh
podman build -t footnote:dev .
podman run --rm --network=host -v ./my.yaml:/etc/footnote/footnote.yaml:ro \
  -e FOOTNOTE_DATABASE_URL=postgres://footnote:footnote@127.0.0.1:25432/footnote footnote:dev serve
```

Discord 機器人只能有一個副本；同一個 token 同時連兩次，每題會回兩次。

## 評估

`footnote eval -f <評估集> --sweep` 對每題只跑一次檢索與重排序，存下分數後在本機把 `τ_answer` 從 0.05 掃到 0.80，
印出每個閾值的**誤導率**（該拒答卻通過閘門的比例）與**誤拒率**（該回答卻被擋下的比例），
並建議「誤導率達標下誤拒率最低」那段區間的中間值。

`eval/eval.example.yaml` 只示範格式，題數遠不夠校準。設定檔裡的門檻只是起點，換了語料要重新掃。

## 目前狀態

| 範圍 | 狀態 |
|---|---|
| 同步、切塊（Markdown／xlsx）、混合檢索、重排、閘門 A、回覆組裝 | 可用 |
| 模型整理回答（閘門 B、C、月預算）、翻譯補查、別名詞彙表 | 可用 |
| 缺口去重、`footnote gaps` 指令、重檢、通知（outbox） | 可用，有整合測試 |
| FAQ 卡提案、Discord 核准、過期偵測、詞彙表審核代理 | 可用，有整合測試 |
| Discord bot、本機測試頁 | 可用 |
| GitLab API 來源、push webhook | **未做**，目前只讀本機 checkout |
| `/metrics`、Kubernetes 部署範本 | **未做** |

## 開發

```sh
make test   # go vet + go test；整合測試連 FOOTNOTE_TEST_DATABASE_URL（會清空該資料庫）
```

整合測試需要一個可以清空的 Postgres + pgvector；`make migrate` 會在 compose 的資料庫裡建好 `footnote_test`。
沒設 `FOOTNOTE_TEST_DATABASE_URL` 時整合測試自動略過。單元測試一律用假的嵌入與重排序，不打真的 TEI。

## 授權

[MIT](LICENSE)
