# 發版流程

Acme 平台的後端服務（`acme-api`、`acme-worker`）固定在每週二、週四下午發版，其他時間只上緊急修正（hotfix）。發版由當週的發版負責人執行，流程如下。

## 上線前檢查

- CI 的 `test`、`lint`、`e2e` 三個 job 都要是綠的，且 MR 至少有一位 reviewer 核可。
- 資料庫 migration 必須向下相容：先加新欄位，等程式都改用新欄位後，下一次發版才移除舊欄位。
- 在 #release 頻道貼出本次的變更清單，可以用 `acmectl changelog --since last-release` 產生。

## 打 tag 與部署

1. 在 `main` 上打語意化版本 tag，例如 `v2.14.0`，推上去：`git push origin v2.14.0`。
2. tag 推上去後，CI 會建置映像檔並推到 registry，接著自動部署到 staging。
3. 在 staging 跑冒煙測試：`acmectl smoke --env staging`。全部通過後執行 `acmectl deploy --env prod --version v2.14.0`。
4. 正式環境採金絲雀部署：先導 10% 流量觀察 15 分鐘，錯誤率沒有上升才切到全量。

## 上線後

- 觀察 Grafana 的「API 總覽」儀表板 30 分鐘，重點看 5xx 比例與 P95 延遲。
- 在 #release 頻道回報完成，並把這次發版的 MR 連結記到發版紀錄。
