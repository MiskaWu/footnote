---
questions:
  - 正式環境要怎麼回滾到上一版？
  - prod 部署壞掉怎麼退回去
  - 怎麼 rollback
sources:
  - acme/handbook:deploy/rollback.md#回滾步驟
  - acme/handbook:deploy/rollback.md#資料庫 migration 怎麼辦
verified: 2026-10-08
---
執行 `acmectl rollback --env prod`，不加 `--to` 就會退回上一個版本，約需 3 分鐘 [1]。
完成後到 Grafana 確認 5xx 比例恢復，並在 #release 頻道公告、開一張事故單 [1]。
回滾不會倒回資料庫 migration；migration 規定向下相容，舊版程式可以直接跑在新的 schema 上 [2]。
