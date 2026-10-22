package ingest

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/miskawu/footnote/internal/config"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
)

// 需要 FOOTNOTE_TEST_DATABASE_URL（會清空該資料庫）；沒設就略過。
// 用暫存目錄當文件來源與 FAQ 來源，從頭走一次增量同步、FAQ 卡入庫、過期偵測。

type constEmbed struct{}

func (constEmbed) Embed(ctx context.Context, texts []string, kind tei.EmbedKind) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const guide = "# 部署\n\n部署流程：先跑測試，再打 tag，最後由 ArgoCD 同步到正式環境。回滾時把 tag 指回上一版。\n"

func card(verified string) string {
	return `---
questions: [怎麼部署？, 部署流程是什麼]
sources: ["test/docs:guide.md#部署"]
gap: 999
verified: ` + verified + `
---
先跑測試、打 tag，再由 ArgoCD 同步 [1]。
`
}

func TestIngestIncrementalFAQAndStale(t *testing.T) {
	url := os.Getenv("FOOTNOTE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FOOTNOTE_TEST_DATABASE_URL 未設定，略過整合測試")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	if err := store.Migrate(ctx, url, 4); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	docs, faqDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(docs, "guide.md"), guide)
	write(t, filepath.Join(docs, "notes.txt"), "不在 include 裡")
	write(t, filepath.Join(faqDir, "deploy.md"), card("2026-10-08"))
	write(t, filepath.Join(faqDir, "README.md"), "# 說明")

	cfg := &config.Config{
		Sources: []config.Source{{Name: "docs", Type: "local", ProjectPath: "test/docs", Dir: docs, Ref: "main", Include: []string{"**/*.md"}, MaxTextKB: 512}},
		FAQ:     &config.Source{Name: "faq", Type: "local", ProjectPath: "test/faq", Dir: faqDir, Ref: "main", Include: []string{"**/*.md"}, Exclude: []string{"README.md"}, MaxTextKB: 512},
	}
	in := &Ingester{Store: st, Embedder: constEmbed{}, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	res, err := in.Run(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Added != 1 || res[1].Added != 1 || res[1].Skipped != 0 {
		t.Fatalf("first run = %+v", res)
	}
	// 卡片引用不存在的缺口 G999：卡片照收，連結拿掉
	f, err := st.GetFAQ(ctx, "test/faq", "deploy.md")
	if err != nil || f.GapID != nil || len(f.Sources) != 1 || f.Sources[0].SrcBlobSHA == "" {
		t.Fatalf("faq = %+v, err = %v", f, err)
	}

	// 沒變就不重做
	res, _ = in.Run(ctx, "", false)
	if res[0].Unchanged != 1 || res[1].Unchanged != 1 || res[0].Changed() || res[1].Changed() {
		t.Fatalf("second run = %+v", res)
	}

	// 來源文件改了 ⇒ 文件重建、卡片過期
	write(t, filepath.Join(docs, "guide.md"), guide+"\n補充：正式環境要先公告。\n")
	res, _ = in.Run(ctx, "", false)
	if res[0].Updated != 1 || len(res[0].StaleFAQs) != 1 {
		t.Fatalf("after doc change = %+v", res)
	}
	listings, _, _ := in.Listings(ctx)
	stale, err := st.ListStaleFAQs(ctx, listings)
	if err != nil || len(stale) != 1 || len(stale[0].Changed) != 1 {
		t.Fatalf("stale = %+v, err = %v", stale, err)
	}

	// 卡片重新確認（改 verified）⇒ 基準換成現況、過期解除
	write(t, filepath.Join(faqDir, "deploy.md"), card("2026-10-09"))
	res, _ = in.Run(ctx, "", false)
	if res[1].Updated != 1 {
		t.Fatalf("after card update = %+v", res)
	}
	if stale, _ := st.ListStaleFAQs(ctx, listings); len(stale) != 0 {
		t.Fatalf("still stale: %+v", stale)
	}

	// 設定改了（處理設定指紋變了）⇒ 內容沒變也重建
	in.Cfg.Sources[0].SkipSheets = []string{"廢棄"}
	res, _ = in.Run(ctx, "", false)
	if res[0].Updated != 1 {
		t.Fatalf("after config change = %+v", res)
	}

	// 檔案刪掉 ⇒ 片段一起刪
	os.Remove(filepath.Join(docs, "guide.md"))
	res, _ = in.Run(ctx, "", false)
	if res[0].Deleted != 1 {
		t.Fatalf("after delete = %+v", res)
	}
}
