---
questions:
  - 資料庫備份要怎麼還原？
  - 誤刪資料可以救回來嗎
sources:
  - acme/handbook:ops/backup.md#還原到新的資料庫
  - acme/handbook:ops/backup.md#備份排程
verified: 2026-10-08
---
先用 `acmectl backup list --env prod` 找快照，再用 `acmectl backup restore --snapshot <快照 ID> --target restore-tmp` 還原到新的執行個體，不要直接覆蓋正式環境 [1]。
正式環境每 5 分鐘上傳一次 WAL，可以加 `--pitr` 還原到任意時間點；每日快照保留 14 天 [2]。
