# TLS 憑證

Acme 平台對外與對內的連線都走 TLS。這份說明憑證平常怎麼自動更新，以及什麼情況下需要人工介入、手動輪替。收到憑證即將到期的告警時，先看「自動更新」一節確認 cert-manager 是否正常運作。

## 自動更新

對外網域（例如 `api.acme.example`）的 TLS 憑證由 cert-manager 透過 ACME 協定自動申請與更新，到期前 30 天會自動續期，平常不需要人工操作。內部服務之間的 mTLS 憑證由服務網格每 24 小時自動輪替一次。

## 手動輪替

只有在私鑰可能外洩，或 cert-manager 異常沒有續期時，才需要手動輪替：

1. 執行 `acmectl cert rotate --domain api.acme.example`，會撤銷舊憑證並申請新的。
2. 確認新憑證已生效：`acmectl cert status --domain api.acme.example`，到期日應該變成 90 天後。
3. 私鑰外洩的情況要另外開資安事故單，並通知資安負責人。
