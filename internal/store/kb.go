package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// KBChange 是 FAQ 卡或詞彙表的一筆修改（kb_changes）。Content 是 nil 表示這筆要刪除。
type KBChange struct {
	ID         int64      `json:"id"`
	Path       string     `json:"path"`
	Content    *string    `json:"content"`
	Note       string     `json:"note"`
	ProposedBy string     `json:"proposed_by"`
	Status     string     `json:"status"`
	Reviewer   string     `json:"reviewer"`
	ReviewNote string     `json:"review_note"`
	ReviewRef  string     `json:"-"`
	ReviewVia  string     `json:"review_via"` // human（Discord）或 agent（審核代理，只有詞彙表）
	CreatedAt  time.Time  `json:"created_at"`
	ReviewedAt *time.Time `json:"reviewed_at"`
}

const kbColumns = `id, path, content, note, proposed_by, status, reviewer, review_note, review_ref, review_via, created_at, reviewed_at`

func scanKB(r pgx.Row) (KBChange, error) {
	var c KBChange
	err := r.Scan(&c.ID, &c.Path, &c.Content, &c.Note, &c.ProposedBy, &c.Status, &c.Reviewer, &c.ReviewNote, &c.ReviewRef, &c.ReviewVia, &c.CreatedAt, &c.ReviewedAt)
	return c, err
}

// KBCurrent 回傳目前線上的內容：每個 path 最新一筆核准的修改，最新一筆是刪除的不列。
func (s *Store) KBCurrent(ctx context.Context) (map[string]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT ON (path) path, content FROM kb_changes
		WHERE status = 'approved' ORDER BY path, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cur := map[string]string{}
	for rows.Next() {
		var p string
		var c *string
		if err := rows.Scan(&p, &c); err != nil {
			return nil, err
		}
		if c != nil {
			cur[p] = *c
		}
	}
	return cur, rows.Err()
}

// KBEmpty 回報 kb_changes 是不是完全沒有紀錄（還沒匯入過）。
func (s *Store) KBEmpty(ctx context.Context) (bool, error) {
	var any bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kb_changes)`).Scan(&any)
	return !any, err
}

// KBPropose 新增一筆待審的修改。同一張卡已有待審的提案時，舊的改成 superseded。
func (s *Store) KBPropose(ctx context.Context, c KBChange) (int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE kb_changes SET status = 'superseded', reviewed_at = now() WHERE path = $1 AND status = 'pending'`, c.Path); err != nil {
		return 0, err
	}
	via := c.ReviewVia
	if via == "" {
		via = "human"
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO kb_changes (path, content, note, proposed_by, review_via) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		c.Path, c.Content, c.Note, c.ProposedBy, via).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// KBPending 回傳這個路徑待審的提案，沒有時回傳 ErrNotFound。
func (s *Store) KBPending(ctx context.Context, path string) (*KBChange, error) {
	c, err := scanKB(s.Pool.QueryRow(ctx, `SELECT `+kbColumns+` FROM kb_changes WHERE path = $1 AND status = 'pending'`, path))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// KBBefore 回傳某筆修改之前的內容：同一路徑比它早、最新一筆核准的修改。之前沒有內容時回傳空字串。
func (s *Store) KBBefore(ctx context.Context, path string, id int64) (string, error) {
	var c *string
	err := s.Pool.QueryRow(ctx, `
		SELECT content FROM kb_changes WHERE path = $1 AND status = 'approved' AND id < $2 ORDER BY id DESC LIMIT 1`, path, id).Scan(&c)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", err
	case c == nil: // 之前那筆是刪除
		return "", nil
	}
	return *c, nil
}

// KBRevert 寫一筆已核准的修改，把代理核准的內容撤回去。這筆是負責人下的指令（kb.RevertGlossary），不經過審核；
// 還在待審的提案是照撤回前的內容寫的，一起取代掉，免得核准時又把撤回的叫法帶回來。
func (s *Store) KBRevert(ctx context.Context, c KBChange) (int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE kb_changes SET status = 'superseded', reviewed_at = now() WHERE path = $1 AND status = 'pending'`, c.Path); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO kb_changes (path, content, note, proposed_by, status, reviewer, reviewed_at)
		VALUES ($1, $2, $3, $4, 'approved', 'revert', now()) RETURNING id`, c.Path, c.Content, c.Note, c.ProposedBy).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// KBImport 把既有內容直接寫成已核准，只給第一次匯入用：已經有紀錄時拒絕，不是繞過審核的後門。
func (s *Store) KBImport(ctx context.Context, docs map[string]string, by string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE kb_changes IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	var any bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kb_changes)`).Scan(&any); err != nil {
		return err
	}
	if any {
		return errors.New("資料庫裡已經有 FAQ 卡的紀錄，匯入只能做一次；之後的修改要走提案")
	}
	for p, c := range docs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO kb_changes (path, content, note, proposed_by, status, reviewer, reviewed_at)
			VALUES ($1, $2, '從目錄匯入', $3, 'approved', 'import', now())`, p, c, by); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) KBGet(ctx context.Context, id int64) (*KBChange, error) {
	c, err := scanKB(s.Pool.QueryRow(ctx, `SELECT `+kbColumns+` FROM kb_changes WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// KBList 列出修改紀錄，新的在前；status 空字串表示全部。
func (s *Store) KBList(ctx context.Context, status string, limit int) ([]KBChange, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+kbColumns+` FROM kb_changes WHERE $1 = '' OR status = $1 ORDER BY id DESC LIMIT $2`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KBChange
	for rows.Next() {
		c, err := scanKB(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// KBUnposted 是還沒貼到審核頻道的提案；審核代理決定的提案不貼。
func (s *Store) KBUnposted(ctx context.Context) ([]KBChange, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+kbColumns+` FROM kb_changes WHERE status = 'pending' AND review_ref = '' AND review_via = 'human' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KBChange
	for rows.Next() {
		c, err := scanKB(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) KBSetReviewRef(ctx context.Context, id int64, ref string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE kb_changes SET review_ref = $2 WHERE id = $1`, id, ref)
	return err
}

// KBDecide 把待審的提案改成 approved 或 rejected。回傳 false 表示它已經不是待審（被審過或被新提案取代）。
func (s *Store) KBDecide(ctx context.Context, id int64, status, reviewer, note string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE kb_changes SET status = $2, reviewer = $3, review_note = $4, reviewed_at = now()
		WHERE id = $1 AND status = 'pending'`, id, status, reviewer, note)
	return tag.RowsAffected() == 1, err
}
