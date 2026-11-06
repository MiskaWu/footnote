package source

import (
	"context"

	"github.com/miskawu/footnote/internal/config"
)

// DocStore 提供 FAQ 卡與詞彙表目前的內容（實作是 store.KBCurrent）。
type DocStore interface {
	KBCurrent(ctx context.Context) (map[string]string, error)
}

// DB 是 FAQ 來源：內容存在資料庫（kb_changes），不是 git repo。對入庫流程來說跟其他來源一樣，
// sha 也用 git blob 的定義，所以從目錄匯入的內容跟原本入庫的 sha 相同，不必重新嵌入。
type DB struct {
	Cfg  config.Source
	Docs DocStore
	cur  map[string]string
}

func NewDB(cfg config.Source, docs DocStore) *DB { return &DB{Cfg: cfg, Docs: docs} }

func (d *DB) Config() config.Source { return d.Cfg }

func (d *DB) load(ctx context.Context) error {
	cur, err := d.Docs.KBCurrent(ctx)
	if err == nil {
		d.cur = cur
	}
	return err
}

func (d *DB) List(ctx context.Context) (map[string]string, error) {
	if err := d.load(ctx); err != nil {
		return nil, err
	}
	files := make(map[string]string, len(d.cur))
	for p, c := range d.cur {
		files[p] = BlobSHA([]byte(c))
	}
	return files, nil
}

// Selected 全部都要索引：能進資料庫的路徑在提案時就檢查過了。
func (d *DB) Selected(all map[string]string) map[string]string {
	out := make(map[string]string, len(all))
	for p, sha := range all {
		out[p] = sha
	}
	return out
}

func (d *DB) IsDraft(string) bool { return false }

func (d *DB) Read(ctx context.Context, path string) ([]byte, error) {
	if d.cur == nil {
		if err := d.load(ctx); err != nil {
			return nil, err
		}
	}
	return []byte(d.cur[path]), nil
}
