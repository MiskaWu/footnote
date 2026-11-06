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

// putKB 直接寫一筆核准的修改，代替「提案、人核准」：這裡測的是入庫，審核在 internal/kb 測。content 是 nil 表示刪除。
func putKB(t *testing.T, st *store.Store, p string, content *string) {
	t.Helper()
	ctx := context.Background()
	id, err := st.KBPropose(ctx, store.KBChange{Path: p, Content: content, ProposedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.KBDecide(ctx, id, "approved", "test", ""); err != nil || !ok {
		t.Fatalf("decide: %v, %v", ok, err)
	}
}

func text(s string) *string { return &s }

const guide = "# 部署\n\n部署流程：先跑測試，再打 tag，最後由 CD 工具同步到正式環境。回滾時把 tag 指回上一版。\n"

func card(verified string) string {
	return `---
questions: [怎麼部署？, 部署流程是什麼]
sources: ["test/docs:guide.md#部署"]
gap: 999
verified: ` + verified + `
---
先跑測試、打 tag，再由 CD 工具同步 [1]。
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

	docs := t.TempDir()
	write(t, filepath.Join(docs, "guide.md"), guide)
	write(t, filepath.Join(docs, "notes.txt"), "不在 include 裡")
	putKB(t, st, "deploy.md", text(card("2026-10-08")))

	cfg := &config.Config{
		Sources: []config.Source{{Name: "docs", Type: "local", ProjectPath: "test/docs", Dir: docs, Ref: "main", Include: []string{"**/*.md"}, MaxTextKB: 512}},
		FAQ:     &config.Source{Name: "faq", Type: config.TypeDB, ProjectPath: "test/faq", Ref: "main"},
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
	putKB(t, st, "deploy.md", text(card("2026-10-09")))
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

// 版本群組：設定一加上去就重建，舊版的片段記下新版路徑、不再標未確認設計，檢索讀得回來。
func TestIngestVersions(t *testing.T) {
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

	docs := t.TempDir()
	write(t, filepath.Join(docs, "09_權限系統_調整版.md"), "# 收回\n\n權限只剩一人時進入 24 小時收回倒數。\n")
	write(t, filepath.Join(docs, "00_權限.md"), "# 收回\n\n舊權限只剩一人時進入 24 小時收回倒數。\n")
	cfg := &config.Config{Sources: []config.Source{{
		Name: "docs", Type: "local", ProjectPath: "test/docs", Dir: docs, Ref: "main",
		Include: []string{"**/*.md"}, Draft: []string{"00_*.md"}, MaxTextKB: 512,
	}}}
	in := &Ingester{Store: st, Embedder: constEmbed{}, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := in.Run(ctx, "", false); err != nil {
		t.Fatal(err)
	}

	in.Cfg.Sources[0].Versions = [][]string{{"09_權限系統_調整版.md", "00_權限.md"}}
	res, err := in.Run(ctx, "", false)
	if err != nil || res[0].Updated != 2 {
		t.Fatalf("after adding versions = %+v, err = %v", res, err)
	}
	cands, err := st.HybridSearch(ctx, []float32{1, 0, 0, 0}, "收回", 10, 10, nil)
	if err != nil || len(cands) != 2 {
		t.Fatalf("cands = %+v, err = %v", cands, err)
	}
	for _, c := range cands {
		switch c.Path {
		case "00_權限.md":
			if c.SupersededBy != "09_權限系統_調整版.md" || c.Draft {
				t.Errorf("old version = %+v, want superseded_by set and draft=false", c)
			}
		case "09_權限系統_調整版.md":
			if c.SupersededBy != "" || c.Draft {
				t.Errorf("latest version = %+v", c)
			}
		}
	}
}

// 詞彙表跟 FAQ 卡一起同步：新增、沒變、壞掉（整份不用、列在略過清單）、修好、刪掉，每一步查詢端讀到的都跟 main 一致。
func TestIngestGlossary(t *testing.T) {
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

	docs := t.TempDir()
	write(t, filepath.Join(docs, "guide.md"), guide)
	putKB(t, st, "glossary.yaml", text("- [回滾, rollback]\n"))
	cfg := &config.Config{
		Sources: []config.Source{{Name: "docs", Type: "local", ProjectPath: "test/docs", Dir: docs, Ref: "main", Include: []string{"**/*.md"}, MaxTextKB: 512}},
		FAQ:     &config.Source{Name: "faq", Type: config.TypeDB, ProjectPath: "test/faq", Ref: "main"},
	}
	in := &Ingester{Store: st, Embedder: constEmbed{}, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	glossaryNow := func() [][]string {
		t.Helper()
		g, err := st.Glossary(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}

	// 新增：算有變動（recheck 才會重跑缺口），查詢端讀得到
	res, err := in.Run(ctx, "", false)
	if err != nil || res[1].Added != 1 || !res[1].Changed() {
		t.Fatalf("first run = %+v, err = %v", res, err)
	}
	if g := glossaryNow(); len(g) != 1 || g[0][0] != "回滾" || g[0][1] != "rollback" {
		t.Fatalf("glossary = %q", g)
	}
	// 沒變：不重做
	if res, _ = in.Run(ctx, "", false); res[1].Unchanged != 1 || res[1].Changed() {
		t.Fatalf("second run = %+v", res)
	}
	// 壞掉（同一個叫法屬於兩組）：整份不用，列在略過清單；不能留著上一版
	putKB(t, st, "glossary.yaml", text("- [回滾, rollback]\n- [Rollback, 回滾作業]\n"))
	if res, _ = in.Run(ctx, "", false); res[1].Skipped != 1 {
		t.Fatalf("bad glossary run = %+v", res)
	}
	if g := glossaryNow(); len(g) != 0 {
		t.Fatalf("bad glossary still applied: %q", g)
	}
	if sk, _ := st.SkippedFiles(ctx); len(sk) != 1 || sk[0].Path != "glossary.yaml" {
		t.Fatalf("skipped = %+v", sk)
	}
	// 修好
	putKB(t, st, "glossary.yaml", text("- [回滾, rollback]\n- [值班輪替, 輪值表]\n"))
	if res, _ = in.Run(ctx, "", false); res[1].Updated != 1 {
		t.Fatalf("fixed glossary run = %+v", res)
	}
	if g := glossaryNow(); len(g) != 2 {
		t.Fatalf("glossary = %q", g)
	}
	// 刪掉：查詢端跟著沒有
	putKB(t, st, "glossary.yaml", nil)
	if res, _ = in.Run(ctx, "", false); res[1].Deleted != 1 {
		t.Fatalf("after delete = %+v", res)
	}
	if g := glossaryNow(); len(g) != 0 {
		t.Fatalf("deleted glossary still applied: %q", g)
	}
}

// 程式換版但還沒跑 footnote faq import：kb_changes 是空的，這時照常同步會把線上的卡片全部刪掉，要擋下來。
// 匯入之後正常刪除的卡片要照刪，kb_changes 有紀錄就不算這種情況。
func TestIngestKeepsCardsBeforeImport(t *testing.T) {
	url := os.Getenv("FOOTNOTE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FOOTNOTE_TEST_DATABASE_URL 未設定，略過整合測試")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, url, 4); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	docs := t.TempDir()
	write(t, filepath.Join(docs, "guide.md"), guide)
	putKB(t, st, "deploy.md", text(card("2026-10-08")))
	cfg := &config.Config{
		Sources: []config.Source{{Name: "docs", Type: "local", ProjectPath: "test/docs", Dir: docs, Ref: "main", Include: []string{"**/*.md"}, MaxTextKB: 512}},
		FAQ:     &config.Source{Name: "faq", Type: config.TypeDB, ProjectPath: "test/faq", Ref: "main"},
	}
	in := &Ingester{Store: st, Embedder: constEmbed{}, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if res, err := in.Run(ctx, "", false); err != nil || res[1].Added != 1 {
		t.Fatalf("first run = %+v, err = %v", res, err)
	}

	if _, err := conn.Exec(ctx, `TRUNCATE kb_changes`); err != nil {
		t.Fatal(err)
	}
	if res, err := in.Run(ctx, "", false); err != nil || res[1].Deleted != 0 {
		t.Fatalf("空的 kb_changes 不能刪線上的卡片：%+v, err = %v", res, err)
	}
	if _, err := st.GetFAQ(ctx, "test/faq", "deploy.md"); err != nil {
		t.Fatalf("卡片不見了：%v", err)
	}

	putKB(t, st, "deploy.md", text(card("2026-10-08")))
	putKB(t, st, "deploy.md", nil)
	if res, err := in.Run(ctx, "", false); err != nil || res[1].Deleted != 1 {
		t.Fatalf("有紀錄的刪除要照刪：%+v, err = %v", res, err)
	}
}
