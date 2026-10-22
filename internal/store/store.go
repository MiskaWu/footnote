// Package store 集中所有 SQL。
package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

//go:embed schema.sql
var schemaSQL string

var ErrNotFound = errors.New("not found")

type Store struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Migrate 建表。用獨立連線，因為 vector 型別要在 CREATE EXTENSION 之後才註冊得到。
func Migrate(ctx context.Context, url string, dim int) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, strings.ReplaceAll(schemaSQL, "{{DIM}}", strconv.Itoa(dim)))
	return err
}

// ---- 同步 ----

type FileMeta struct {
	ProjectPath string
	Path        string
	Ref         string
	BlobSHA     string
	Kind        string // doc | faq
	Draft       bool
	Skipped     string // 非空表示略過
	Pipeline    string // 處理設定指紋（見 ingest.Fingerprint）
}

type ChunkRow struct {
	Heading   string
	LineStart int
	LineEnd   int
	Content   string
	Embedding []float32
	TSV       string // 已切詞的字串，交給 to_tsvector('simple', …)
}

type FAQRow struct {
	GapID      *int64
	Verified   time.Time
	StaleSince *time.Time // 只在卡片內容沒變、重新嵌入時沿用
	Sources    []FAQSourceRow
}

type FAQSourceRow struct {
	Ord        int
	Ref        string
	SrcProject string
	SrcPath    string
	SrcBlobSHA string
}

// SyncedFile 是某檔案上次同步時的 blob sha 與處理設定指紋。
type SyncedFile struct {
	BlobSHA  string
	Pipeline string
}

// ListFiles 回傳某專案已同步的檔案。
func (s *Store) ListFiles(ctx context.Context, project string) (map[string]SyncedFile, error) {
	rows, err := s.Pool.Query(ctx, `SELECT path, blob_sha, pipeline FROM source_files WHERE project_path = $1`, project)
	if err != nil {
		return nil, err
	}
	out := map[string]SyncedFile{}
	for rows.Next() {
		var p string
		var f SyncedFile
		if err := rows.Scan(&p, &f.BlobSHA, &f.Pipeline); err != nil {
			return nil, err
		}
		out[p] = f
	}
	return out, rows.Err()
}

// ReplaceFile 在單一 transaction 內「刪舊片段、寫新片段、更新 blob_sha」，FAQ 卡一併更新。
func (s *Store) ReplaceFile(ctx context.Context, f FileMeta, chunks []ChunkRow, faq *FAQRow) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var skipped *string
		if f.Skipped != "" {
			skipped = &f.Skipped
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO source_files (project_path, path, ref, blob_sha, synced_at, skipped, pipeline)
			VALUES ($1, $2, $3, $4, now(), $5, $6)
			ON CONFLICT (project_path, path) DO UPDATE
			SET ref = EXCLUDED.ref, blob_sha = EXCLUDED.blob_sha, synced_at = now(), skipped = EXCLUDED.skipped, pipeline = EXCLUDED.pipeline`,
			f.ProjectPath, f.Path, f.Ref, f.BlobSHA, skipped, f.Pipeline); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE project_path = $1 AND path = $2`, f.ProjectPath, f.Path); err != nil {
			return err
		}
		batch := &pgx.Batch{}
		for _, c := range chunks {
			batch.Queue(`
				INSERT INTO chunks (project_path, path, ref, blob_sha, kind, draft, heading_path, line_start, line_end, content, embedding, tsv)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, to_tsvector('simple', $12))`,
				f.ProjectPath, f.Path, f.Ref, f.BlobSHA, f.Kind, f.Draft, c.Heading, c.LineStart, c.LineEnd, c.Content,
				pgvector.NewVector(c.Embedding), c.TSV)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM faqs WHERE project_path = $1 AND path = $2`, f.ProjectPath, f.Path); err != nil {
			return err
		}
		if faq == nil {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO faqs (project_path, path, blob_sha, gap_id, verified, stale_since) VALUES ($1, $2, $3, $4, $5, $6)`,
			f.ProjectPath, f.Path, f.BlobSHA, faq.GapID, faq.Verified, faq.StaleSince); err != nil {
			return err
		}
		for _, src := range faq.Sources {
			if _, err := tx.Exec(ctx, `
				INSERT INTO faq_sources (faq_project, faq_path, ord, ref, src_project, src_path, src_blob_sha)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				f.ProjectPath, f.Path, src.Ord, src.Ref, src.SrcProject, src.SrcPath, src.SrcBlobSHA); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeleteFile(ctx context.Context, project, path string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM source_files WHERE project_path = $1 AND path = $2`, project, path)
	return err
}

type SkippedFile struct {
	ProjectPath, Path, Reason string
}

func (s *Store) SkippedFiles(ctx context.Context) ([]SkippedFile, error) {
	rows, err := s.Pool.Query(ctx, `SELECT project_path, path, skipped FROM source_files WHERE skipped IS NOT NULL ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SkippedFile, error) {
		var f SkippedFile
		return f, r.Scan(&f.ProjectPath, &f.Path, &f.Reason)
	})
}

// TryLock 取 session 級的 advisory lock；拿不到回傳 ok=false。release 釋放鎖並歸還連線。
func (s *Store) TryLock(ctx context.Context, key int64) (release func(), ok bool, err error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil || !ok {
		conn.Release()
		return nil, false, err
	}
	return func() {
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	}, true, nil
}

// ---- 檢索 ----

type Candidate struct {
	ID          int64
	ProjectPath string
	Path        string
	Ref         string
	Kind        string
	Draft       bool
	Heading     string
	LineStart   int
	LineEnd     int
	Content     string
	RRF         float64
}

// HybridSearch：向量 top k + FTS top k，RRF（k=60）合併取前 limit。
// prefixes 非空時只搜這些專案前綴（頻道權限）。
func (s *Store) HybridSearch(ctx context.Context, vec []float32, ftsQuery string, k, limit int, prefixes []string) ([]Candidate, error) {
	var patterns []string
	for _, p := range prefixes {
		patterns = append(patterns, strings.ReplaceAll(p, "%", `\%`)+"%")
	}
	var filter any
	if len(patterns) > 0 {
		filter = patterns
	}
	var out []Candidate
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = relaxed_order`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			WITH v AS (
				SELECT id, row_number() OVER (ORDER BY d) AS r FROM (
					SELECT id, embedding <=> $1 AS d FROM chunks
					WHERE $3::text[] IS NULL OR project_path LIKE ANY ($3::text[])
					ORDER BY d LIMIT $4
				) s
			), f AS (
				SELECT id, row_number() OVER (ORDER BY rank DESC) AS r FROM (
					SELECT id, ts_rank_cd(tsv, q) AS rank
					FROM chunks, websearch_to_tsquery('simple', $2) q
					WHERE tsv @@ q AND ($3::text[] IS NULL OR project_path LIKE ANY ($3::text[]))
					ORDER BY rank DESC LIMIT $4
				) s
			)
			SELECT c.id, c.project_path, c.path, c.ref, c.kind, c.draft, c.heading_path, c.line_start, c.line_end, c.content,
			       COALESCE(1.0 / (60 + v.r), 0) + COALESCE(1.0 / (60 + f.r), 0) AS rrf
			FROM chunks c
			LEFT JOIN v ON v.id = c.id
			LEFT JOIN f ON f.id = c.id
			WHERE v.id IS NOT NULL OR f.id IS NOT NULL
			ORDER BY rrf DESC
			LIMIT $5`,
			pgvector.NewVector(vec), ftsQuery, filter, k, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, scanCandidate)
		return err
	})
	return out, err
}

func scanCandidate(r pgx.CollectableRow) (Candidate, error) {
	var c Candidate
	err := r.Scan(&c.ID, &c.ProjectPath, &c.Path, &c.Ref, &c.Kind, &c.Draft, &c.Heading, &c.LineStart, &c.LineEnd, &c.Content, &c.RRF)
	return c, err
}

// ---- FAQ ----

type FAQ struct {
	ProjectPath string
	Path        string
	GapID       *int64
	Verified    time.Time
	StaleSince  *time.Time
	Sources     []FAQSourceRow
}

func (s *Store) GetFAQ(ctx context.Context, project, path string) (*FAQ, error) {
	f := &FAQ{ProjectPath: project, Path: path}
	err := s.Pool.QueryRow(ctx, `SELECT gap_id, verified, stale_since FROM faqs WHERE project_path = $1 AND path = $2`, project, path).
		Scan(&f.GapID, &f.Verified, &f.StaleSince)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT ord, ref, src_project, src_path, src_blob_sha FROM faq_sources
		WHERE faq_project = $1 AND faq_path = $2 ORDER BY ord`, project, path)
	if err != nil {
		return nil, err
	}
	f.Sources, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (FAQSourceRow, error) {
		var x FAQSourceRow
		return x, r.Scan(&x.Ord, &x.Ref, &x.SrcProject, &x.SrcPath, &x.SrcBlobSHA)
	})
	return f, err
}

// MarkStaleFAQs 比對某專案的現況，把來源 sha 變了或檔案消失的 FAQ 卡標成過期。回傳新標記的卡。
func (s *Store) MarkStaleFAQs(ctx context.Context, srcProject string, current map[string]string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT fs.faq_project, fs.faq_path, fs.src_path, fs.src_blob_sha
		FROM faq_sources fs JOIN faqs f ON f.project_path = fs.faq_project AND f.path = fs.faq_path
		WHERE fs.src_project = $1 AND f.stale_since IS NULL`, srcProject)
	if err != nil {
		return nil, err
	}
	type key struct{ project, path string }
	stale := map[key]bool{}
	for rows.Next() {
		var fp, fpath, sp, sha string
		if err := rows.Scan(&fp, &fpath, &sp, &sha); err != nil {
			return nil, err
		}
		if cur, ok := current[sp]; !ok || cur != sha {
			stale[key{fp, fpath}] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var marked []string
	for k := range stale {
		if _, err := s.Pool.Exec(ctx, `UPDATE faqs SET stale_since = now() WHERE project_path = $1 AND path = $2 AND stale_since IS NULL`, k.project, k.path); err != nil {
			return nil, err
		}
		marked = append(marked, k.path)
	}
	return marked, nil
}

type StaleFAQ struct {
	ProjectPath string    `json:"project_path"`
	Path        string    `json:"path"`
	StaleSince  time.Time `json:"stale_since"`
	Verified    time.Time `json:"verified"`
	Changed     []string  `json:"changed_sources"` // 已變動或消失的來源
	GapID       *int64    `json:"gap_id,omitempty"`
}

// ListStaleFAQs 列出過期的卡，以及哪些來源變了（current 是各專案現在的 path → sha）。
func (s *Store) ListStaleFAQs(ctx context.Context, current map[string]map[string]string) ([]StaleFAQ, error) {
	rows, err := s.Pool.Query(ctx, `SELECT project_path, path, stale_since, verified, gap_id FROM faqs WHERE stale_since IS NOT NULL ORDER BY stale_since`)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (StaleFAQ, error) {
		var f StaleFAQ
		return f, r.Scan(&f.ProjectPath, &f.Path, &f.StaleSince, &f.Verified, &f.GapID)
	})
	if err != nil {
		return nil, err
	}
	for i := range list {
		faq, err := s.GetFAQ(ctx, list[i].ProjectPath, list[i].Path)
		if err != nil {
			return nil, err
		}
		for _, src := range faq.Sources {
			if cur, ok := current[src.SrcProject][src.SrcPath]; !ok || cur != src.SrcBlobSHA {
				list[i].Changed = append(list[i].Changed, src.Ref)
			}
		}
	}
	return list, nil
}

// FAQsForGap 回傳指向某缺口的 FAQ 卡（已入庫）。
func (s *Store) FAQsForGap(ctx context.Context, gapID int64) ([]FAQ, error) {
	rows, err := s.Pool.Query(ctx, `SELECT project_path, path FROM faqs WHERE gap_id = $1`, gapID)
	if err != nil {
		return nil, err
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var k [2]string
		return k, r.Scan(&k[0], &k[1])
	})
	if err != nil {
		return nil, err
	}
	var out []FAQ
	for _, k := range keys {
		f, err := s.GetFAQ(ctx, k[0], k[1])
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, nil
}

// FAQChunk 回傳某張卡的片段（卡片整張一塊）。
func (s *Store) FAQChunk(ctx context.Context, project, path string) (*Candidate, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, project_path, path, ref, kind, draft, heading_path, line_start, line_end, content, 0::float8
		FROM chunks WHERE project_path = $1 AND path = $2 ORDER BY id LIMIT 1`, project, path)
	if err != nil {
		return nil, err
	}
	c, err := pgx.CollectOneRow(rows, scanCandidate)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// ---- 缺口 ----

type Gap struct {
	ID          int64           `json:"id"`
	Question    string          `json:"question"`
	Kind        string          `json:"kind"`
	Status      string          `json:"status"`
	Hits        int             `json:"hits"`
	Note        string          `json:"note,omitempty"`
	FAQPath     *string         `json:"faq_path,omitempty"`
	NearMisses  json.RawMessage `json:"near_misses"`
	CreatedAt   time.Time       `json:"created_at"`
	LastAskedAt time.Time       `json:"last_asked_at"`
	LastCheckAt *time.Time      `json:"last_check_at,omitempty"`
	NotifiedAt  *time.Time      `json:"notified_at,omitempty"`
}

const gapCols = `id, question, kind, status, hits, note, faq_path, near_misses, created_at, last_asked_at, last_check_at, notified_at`

func scanGap(r pgx.CollectableRow) (Gap, error) {
	var g Gap
	err := r.Scan(&g.ID, &g.Question, &g.Kind, &g.Status, &g.Hits, &g.Note, &g.FAQPath, &g.NearMisses, &g.CreatedAt, &g.LastAskedAt, &g.LastCheckAt, &g.NotifiedAt)
	return g, err
}

// NearestGap 回傳最相似、尚未 closed 的缺口與其 cosine 相似度。
func (s *Store) NearestGap(ctx context.Context, vec []float32) (*Gap, float64, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+gapCols+`, 1 - (q_embedding <=> $1) AS sim
		FROM gaps WHERE status <> 'closed' ORDER BY q_embedding <=> $1 LIMIT 1`, pgvector.NewVector(vec))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, 0, rows.Err()
	}
	var g Gap
	var sim float64
	if err := rows.Scan(&g.ID, &g.Question, &g.Kind, &g.Status, &g.Hits, &g.Note, &g.FAQPath, &g.NearMisses, &g.CreatedAt, &g.LastAskedAt, &g.LastCheckAt, &g.NotifiedAt, &sim); err != nil {
		return nil, 0, err
	}
	return &g, sim, nil
}

func (s *Store) InsertGap(ctx context.Context, question, kind string, vec []float32, nearMisses any, faqPath *string) (*Gap, error) {
	nm, err := json.Marshal(nearMisses)
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `
		INSERT INTO gaps (question, q_embedding, kind, near_misses, faq_path) VALUES ($1, $2, $3, $4, $5)
		RETURNING `+gapCols, question, pgvector.NewVector(vec), kind, nm, faqPath)
	if err != nil {
		return nil, err
	}
	g, err := pgx.CollectOneRow(rows, scanGap)
	return &g, err
}

func (s *Store) BumpGap(ctx context.Context, id int64) (*Gap, error) {
	rows, err := s.Pool.Query(ctx, `UPDATE gaps SET hits = hits + 1, last_asked_at = now() WHERE id = $1 RETURNING `+gapCols, id)
	if err != nil {
		return nil, err
	}
	g, err := pgx.CollectOneRow(rows, scanGap)
	return &g, err
}

func (s *Store) GetGap(ctx context.Context, id int64) (*Gap, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+gapCols+` FROM gaps WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	g, err := pgx.CollectOneRow(rows, scanGap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &g, err
}

func (s *Store) SetGapStatus(ctx context.Context, id int64, status string, note *string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE gaps SET status = $2, note = COALESCE($3, note), last_check_at = now() WHERE id = $1`, id, status, note)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) TouchGapChecked(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE gaps SET last_check_at = now() WHERE id = $1`, id)
	return err
}

func (s *Store) GapExists(ctx context.Context, id int64) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM gaps WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}

// MarkGapNotified 回傳 false 表示已經通知過（每個缺口只通知一次）。
func (s *Store) MarkGapNotified(ctx context.Context, id int64) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE gaps SET notified_at = now() WHERE id = $1 AND notified_at IS NULL`, id)
	return err == nil && tag.RowsAffected() == 1, err
}

func (s *Store) ListGaps(ctx context.Context, statuses []string) ([]Gap, error) {
	var rows pgx.Rows
	var err error
	if len(statuses) == 0 {
		rows, err = s.Pool.Query(ctx, `SELECT `+gapCols+` FROM gaps ORDER BY hits DESC, id`)
	} else {
		rows, err = s.Pool.Query(ctx, `SELECT `+gapCols+` FROM gaps WHERE status = ANY($1) ORDER BY hits DESC, id`, statuses)
	}
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanGap)
}

// ---- 發問 ----

type Ask struct {
	ID         int64
	GapID      *int64
	Platform   string
	ChannelRef string
	ThreadRef  string
	UserRef    string
	Question   string
	Query      string
	Status     string
	Reason     string
	TopScore   float32
	ChunkIDs   []int64
	Reply      string
	LatencyMS  int
	Feedback   *int16
	CreatedAt  time.Time
}

func (s *Store) InsertAsk(ctx context.Context, a Ask) (int64, error) {
	var id int64
	var reason *string
	if a.Reason != "" {
		reason = &a.Reason
	}
	if a.ChunkIDs == nil { // nil slice 會被編碼成 NULL
		a.ChunkIDs = []int64{}
	}
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO asks (platform, channel_ref, thread_ref, user_ref, question, query, status, reason, top_score, chunk_ids, latency_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
		a.Platform, a.ChannelRef, a.ThreadRef, a.UserRef, a.Question, a.Query, a.Status, reason, a.TopScore, a.ChunkIDs, a.LatencyMS).Scan(&id)
	return id, err
}

func (s *Store) SetAskGap(ctx context.Context, askID, gapID int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE asks SET gap_id = $2 WHERE id = $1`, askID, gapID)
	return err
}

func (s *Store) SetAskReply(ctx context.Context, askID int64, reply string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE asks SET reply = $2 WHERE id = $1`, askID, reply)
	return err
}

func (s *Store) SetAskFeedback(ctx context.Context, askID int64, vote int16) error {
	_, err := s.Pool.Exec(ctx, `UPDATE asks SET feedback = $2 WHERE id = $1`, askID, vote)
	return err
}

const askCols = `id, gap_id, platform, channel_ref, thread_ref, user_ref, question, query, status, COALESCE(reason, ''), COALESCE(top_score, 0), chunk_ids, COALESCE(reply, ''), COALESCE(latency_ms, 0), feedback, created_at`

func scanAsk(r pgx.CollectableRow) (Ask, error) {
	var a Ask
	err := r.Scan(&a.ID, &a.GapID, &a.Platform, &a.ChannelRef, &a.ThreadRef, &a.UserRef, &a.Question, &a.Query, &a.Status, &a.Reason, &a.TopScore, &a.ChunkIDs, &a.Reply, &a.LatencyMS, &a.Feedback, &a.CreatedAt)
	return a, err
}

func (s *Store) GetAsk(ctx context.Context, id int64) (*Ask, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+askCols+` FROM asks WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectOneRow(rows, scanAsk)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}

// RecentQuestions 回傳同一討論串最近 n 個問題，由舊到新。
func (s *Store) RecentQuestions(ctx context.Context, platform, threadRef string, n int) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT question FROM (
			SELECT question, created_at FROM asks WHERE platform = $1 AND thread_ref = $2 ORDER BY created_at DESC LIMIT $3
		) s ORDER BY created_at`, platform, threadRef, n)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *Store) AsksForGap(ctx context.Context, gapID int64) ([]Ask, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+askCols+` FROM asks WHERE gap_id = $1 ORDER BY id`, gapID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanAsk)
}

// ---- 通知 outbox ----

type Notice struct {
	ID         int64      `json:"id"`
	GapID      *int64     `json:"gap_id,omitempty"`
	Platform   string     `json:"platform"`
	ChannelRef string     `json:"channel_ref"`
	ThreadRef  string     `json:"thread_ref"`
	UserRef    string     `json:"user_ref"`
	Body       string     `json:"body"`
	CreatedAt  time.Time  `json:"created_at"`
	SentAt     *time.Time `json:"sent_at,omitempty"`
}

func (s *Store) EnqueueNotice(ctx context.Context, n Notice) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO notices (gap_id, platform, channel_ref, thread_ref, user_ref, body) VALUES ($1, $2, $3, $4, $5, $6)`,
		n.GapID, n.Platform, n.ChannelRef, n.ThreadRef, n.UserRef, n.Body)
	return err
}

const noticeCols = `id, gap_id, platform, channel_ref, thread_ref, user_ref, body, created_at, sent_at`

func scanNotice(r pgx.CollectableRow) (Notice, error) {
	var n Notice
	err := r.Scan(&n.ID, &n.GapID, &n.Platform, &n.ChannelRef, &n.ThreadRef, &n.UserRef, &n.Body, &n.CreatedAt, &n.SentAt)
	return n, err
}

func (s *Store) PendingNotices(ctx context.Context, platform string) ([]Notice, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+noticeCols+` FROM notices WHERE platform = $1 AND sent_at IS NULL ORDER BY id LIMIT 50`, platform)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanNotice)
}

func (s *Store) MarkNoticeSent(ctx context.Context, id int64, sendErr error) error {
	if sendErr != nil {
		_, err := s.Pool.Exec(ctx, `UPDATE notices SET error = $2 WHERE id = $1`, id, sendErr.Error())
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE notices SET sent_at = now(), error = NULL WHERE id = $1`, id)
	return err
}

// ThreadNotices 回傳某討論串 id 大於 after 的通知（給本機測試頁輪詢用）。
func (s *Store) ThreadNotices(ctx context.Context, platform, threadRef string, after int64) ([]Notice, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+noticeCols+` FROM notices WHERE platform = $1 AND thread_ref = $2 AND id > $3 ORDER BY id`, platform, threadRef, after)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanNotice)
}

// ---- 統計 ----

func (s *Store) Counts(ctx context.Context) (files, chunks, skipped int, err error) {
	err = s.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM source_files), (SELECT count(*) FROM chunks),
		       (SELECT count(*) FROM source_files WHERE skipped IS NOT NULL)`).Scan(&files, &chunks, &skipped)
	return
}
