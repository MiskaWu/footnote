-- footnote schema。向量維度由設定決定（{{DIM}}）。
CREATE EXTENSION IF NOT EXISTS vector;

-- 已同步的檔案（增量同步的依據）
CREATE TABLE IF NOT EXISTS source_files (
  project_path text        NOT NULL,          -- group/sub/project
  path         text        NOT NULL,
  ref          text        NOT NULL,
  blob_sha     text        NOT NULL,
  synced_at    timestamptz NOT NULL DEFAULT now(),
  skipped      text,                          -- 非 NULL 表示被略過的原因（secret、too_large、bad_faq、parse_error…）
  PRIMARY KEY (project_path, path)
);

CREATE TABLE IF NOT EXISTS chunks (
  id           bigserial    PRIMARY KEY,
  project_path text         NOT NULL,
  path         text         NOT NULL,
  ref          text         NOT NULL,
  blob_sha     text         NOT NULL,
  kind         text         NOT NULL DEFAULT 'doc',   -- doc | faq
  draft        boolean      NOT NULL DEFAULT false,   -- 草稿（設定的 draft 樣式命中）
  heading_path text         NOT NULL,                 -- Markdown 標題路徑；xlsx 分頁名；FAQ 代表問法
  line_start   int          NOT NULL,                 -- Markdown 行號；xlsx 列號
  line_end     int          NOT NULL,
  content      text         NOT NULL,
  acl          text[]       NOT NULL DEFAULT '{all}',
  embedding    vector({{DIM}}) NOT NULL,
  tsv          tsvector     NOT NULL,
  updated_at   timestamptz  NOT NULL DEFAULT now(),
  FOREIGN KEY (project_path, path) REFERENCES source_files (project_path, path) ON DELETE CASCADE
);
-- 處理設定指紋：抽文字、切塊的設定改了，內容沒變的檔案也要重建（2026-10-08 加）
ALTER TABLE source_files ADD COLUMN IF NOT EXISTS pipeline text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS chunks_embedding_idx ON chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS chunks_tsv_idx ON chunks USING gin (tsv);
CREATE INDEX IF NOT EXISTS chunks_file_idx ON chunks (project_path, path);

-- 知識缺口（相似問題合併成一筆）。狀態只存在這裡，用 footnote gaps 指令或 recheck 改。
CREATE TABLE IF NOT EXISTS gaps (
  id            bigserial    PRIMARY KEY,
  question      text         NOT NULL,
  q_embedding   vector({{DIM}}) NOT NULL,
  kind          text         NOT NULL,                 -- no_data | unhelpful | wrong_answer
  status        text         NOT NULL DEFAULT 'open',  -- open | maybe_resolved | needs_human | closed | dismissed
  hits          int          NOT NULL DEFAULT 1,
  note          text         NOT NULL DEFAULT '',
  faq_path      text,
  near_misses   jsonb        NOT NULL DEFAULT '[]',
  created_at    timestamptz  NOT NULL DEFAULT now(),
  last_asked_at timestamptz  NOT NULL DEFAULT now(),
  last_check_at timestamptz,
  notified_at   timestamptz
);
CREATE INDEX IF NOT EXISTS gaps_embedding_idx ON gaps USING hnsw (q_embedding vector_cosine_ops);

-- FAQ 卡（內容本身照常進 source_files / chunks）
CREATE TABLE IF NOT EXISTS faqs (
  project_path text        NOT NULL,
  path         text        NOT NULL,
  blob_sha     text        NOT NULL,
  gap_id       bigint      REFERENCES gaps (id) ON DELETE SET NULL,
  verified     date        NOT NULL,
  stale_since  timestamptz,
  PRIMARY KEY (project_path, path),
  FOREIGN KEY (project_path, path) REFERENCES source_files (project_path, path) ON DELETE CASCADE
);

-- FAQ 卡引用的來源，以及卡片入庫當下來源的 sha（過期偵測的基準）
CREATE TABLE IF NOT EXISTS faq_sources (
  faq_project  text NOT NULL,
  faq_path     text NOT NULL,
  ord          int  NOT NULL,                 -- 在 sources 裡的順序（1 起算），對應本文的 [n]
  ref          text NOT NULL,                 -- 原始寫法 project_path:path#anchor
  src_project  text NOT NULL,
  src_path     text NOT NULL,
  src_blob_sha text NOT NULL,
  PRIMARY KEY (faq_project, faq_path, ord),
  FOREIGN KEY (faq_project, faq_path) REFERENCES faqs (project_path, path) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS faq_sources_src_idx ON faq_sources (src_project, src_path);

-- 每次發問
CREATE TABLE IF NOT EXISTS asks (
  id           bigserial   PRIMARY KEY,
  gap_id       bigint      REFERENCES gaps (id) ON DELETE SET NULL,
  platform     text        NOT NULL,          -- discord | web | cli
  channel_ref  text        NOT NULL,
  thread_ref   text        NOT NULL,
  user_ref     text        NOT NULL,
  question     text        NOT NULL,
  query        text        NOT NULL,          -- 實際拿去檢索的字串（追問時含前一問）
  status       text        NOT NULL,          -- faq | docs | no_data
  reason       text,                          -- gate_a | error
  top_score    real,
  chunk_ids    bigint[]    NOT NULL DEFAULT '{}',
  reply        text,
  latency_ms   int,
  feedback     smallint,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS asks_thread_idx ON asks (platform, thread_ref, created_at);
CREATE INDEX IF NOT EXISTS asks_gap_idx ON asks (gap_id);

-- 待送出的通知（outbox）。程式在哪裡觸發通知都只寫這張表，由 serve 依平台送出，
-- 所以 CLI 下的 footnote gaps close 也能通知到 Discord。
CREATE TABLE IF NOT EXISTS notices (
  id          bigserial   PRIMARY KEY,
  gap_id      bigint      REFERENCES gaps (id) ON DELETE SET NULL,
  platform    text        NOT NULL,
  channel_ref text        NOT NULL,
  thread_ref  text        NOT NULL,
  user_ref    text        NOT NULL,
  body        text        NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  sent_at     timestamptz,
  error       text
);
CREATE INDEX IF NOT EXISTS notices_pending_idx ON notices (platform) WHERE sent_at IS NULL;
CREATE INDEX IF NOT EXISTS notices_thread_idx ON notices (platform, thread_ref, id);
