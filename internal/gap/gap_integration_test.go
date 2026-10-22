package gap

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/miskawu/footnote/internal/config"
	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/render"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
	"github.com/miskawu/footnote/internal/textseg"
)

// 需要一個可以清空的 Postgres（含 pgvector）：
//   FOOTNOTE_TEST_DATABASE_URL=postgres://footnote:footnote@127.0.0.1:25432/footnote_test go test ./internal/gap/
// 沒設就略過，所以沒有資料庫的環境不會紅。

// 假嵌入：依關鍵字給向量，同主題的問題 cosine = 1，不同主題 = 0。
type kwEmbed struct{}

func vecFor(s string) []float32 {
	switch {
	case strings.Contains(s, "憑證"):
		return []float32{1, 0, 0, 0}
	case strings.Contains(s, "備份"):
		return []float32{0, 1, 0, 0}
	case strings.Contains(s, "errgroup"):
		return []float32{0, 0, 1, 0}
	}
	return []float32{0, 0, 0, 1}
}

func (kwEmbed) Embed(ctx context.Context, texts []string, kind tei.EmbedKind) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = vecFor(t)
	}
	return out, nil
}

// 假重排：文件內容含問題的主題詞就高分。
type kwRerank struct{}

func (kwRerank) Rerank(ctx context.Context, q string, docs []string) ([]float32, error) {
	out := make([]float32, len(docs))
	for i, d := range docs {
		for _, kw := range []string{"憑證", "備份"} {
			if strings.Contains(q, kw) && strings.Contains(d, kw) {
				out[i] = 0.9
			}
		}
	}
	return out, nil
}

func setup(t *testing.T) (*Service, *store.Store) {
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
	t.Cleanup(st.Close)
	engine := &qa.Engine{Store: st, Embedder: kwEmbed{}, Reranker: kwRerank{},
		Cfg: config.QA{Candidates: 30, KReply: 3, TauAnswer: 0.35, TauKeep: 0.15}}
	svc := &Service{Store: st, QA: engine, TauDup: 0.85, Render: render.Options{GitLabURL: "https://g", ExcerptChars: 50},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return svc, st
}

func addDoc(t *testing.T, st *store.Store, project, path, content string) {
	t.Helper()
	err := st.ReplaceFile(context.Background(),
		store.FileMeta{ProjectPath: project, Path: path, Ref: "main", BlobSHA: "sha-" + path, Kind: "doc"},
		[]store.ChunkRow{{Heading: "h", LineStart: 1, LineEnd: 1, Content: content, Embedding: vecFor(content), TSV: textseg.Document(content)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func ask(t *testing.T, svc *Service, thread, q string) *store.Gap {
	t.Helper()
	ctx := context.Background()
	id, err := svc.Store.InsertAsk(ctx, store.Ask{Platform: "web", ChannelRef: "web", ThreadRef: thread, UserRef: "u", Question: q, Query: q, Status: "no_data"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := svc.Record(ctx, id, q, vecFor(q), KindNoData, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func pending(t *testing.T, st *store.Store) []store.Notice {
	t.Helper()
	n, err := st.PendingNotices(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDedupDismissAndRecheck(t *testing.T) {
	svc, st := setup(t)
	ctx := context.Background()

	// 三種問法、同一主題 ⇒ 一個缺口、hits=3
	g1 := ask(t, svc, "t1", "憑證要怎麼更新？")
	g2 := ask(t, svc, "t2", "憑證更新的指令在哪")
	g3 := ask(t, svc, "t3", "怎麼換憑證")
	if g1.ID != g2.ID || g2.ID != g3.ID || g3.Hits != 3 {
		t.Fatalf("dedup failed: %d %d %d hits=%d", g1.ID, g2.ID, g3.ID, g3.Hits)
	}

	// dismissed：相似問題只加 hits、不建新缺口、狀態不變
	gen := ask(t, svc, "t4", "errgroup 怎麼用")
	if err := svc.Dismiss(ctx, gen.ID); err != nil {
		t.Fatal(err)
	}
	again := ask(t, svc, "t5", "Go errgroup 用法")
	if again.ID != gen.ID || again.Status != StatusDismissed || again.Hits != 2 {
		t.Fatalf("dismissed gap = %+v", again)
	}

	// 文件還沒補：recheck 不動
	if sum, err := svc.Recheck(ctx); err != nil || sum.MaybeResolved != 0 {
		t.Fatalf("recheck before doc: %+v %v", sum, err)
	}
	// 補了文件：no_data 缺口變 maybe_resolved，三個討論串各一則通知
	addDoc(t, st, "acme/handbook", "certs.md", "憑證用 acmectl cert rotate 更新。")
	sum, err := svc.Recheck(ctx)
	if err != nil || sum.MaybeResolved != 1 {
		t.Fatalf("recheck after doc: %+v %v", sum, err)
	}
	g, _ := st.GetGap(ctx, g1.ID)
	if g.Status != StatusMaybeResolved || g.NotifiedAt == nil {
		t.Fatalf("gap = %+v", g)
	}
	if n := pending(t, st); len(n) != 3 || !strings.Contains(n[0].Body, "這題現在有答案了") {
		t.Fatalf("notices = %+v", n)
	}
	// 再 recheck 不重複通知
	if _, err := svc.Recheck(ctx); err != nil {
		t.Fatal(err)
	}
	if n := pending(t, st); len(n) != 3 {
		t.Fatalf("duplicate notices: %d", len(n))
	}
}

func TestUnhelpfulNotAutoResolvedButFAQClosesIt(t *testing.T) {
	svc, st := setup(t)
	ctx := context.Background()
	addDoc(t, st, "acme/handbook", "backup.md", "備份排程說明。")

	// 回了文件段落但被按 👎 ⇒ unhelpful；閘門 A 本來就會過，recheck 不能自動標成解決
	id, err := st.InsertAsk(ctx, store.Ask{Platform: "web", ChannelRef: "web", ThreadRef: "t1", UserRef: "u", Question: "備份怎麼還原", Query: "備份怎麼還原", Status: "docs"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := svc.Record(ctx, id, "備份怎麼還原", vecFor("備份"), KindUnhelpful, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum, _ := svc.Recheck(ctx); sum.MaybeResolved != 0 {
		t.Fatalf("unhelpful gap auto-resolved: %+v", sum)
	}

	// FAQ 卡指向這個缺口 ⇒ closed 並通知卡片答案
	gid := g.ID
	err = st.ReplaceFile(ctx,
		store.FileMeta{ProjectPath: "acme/faq", Path: "backup/restore.md", Ref: "main", BlobSHA: "faq1", Kind: "faq"},
		[]store.ChunkRow{{Heading: "備份怎麼還原", LineStart: 7, LineEnd: 7, Content: "用 acmectl backup restore 指定快照還原 [1]。", Embedding: vecFor("備份"), TSV: textseg.Document("備份還原")}},
		&store.FAQRow{GapID: &gid, Verified: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), Sources: []store.FAQSourceRow{
			{Ord: 1, Ref: "acme/handbook:backup.md", SrcProject: "acme/handbook", SrcPath: "backup.md", SrcBlobSHA: "sha-backup.md"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	sum, err := svc.Recheck(ctx)
	if err != nil || sum.ClosedByFAQ != 1 {
		t.Fatalf("recheck: %+v %v", sum, err)
	}
	if g, _ := st.GetGap(ctx, gid); g.Status != StatusClosed {
		t.Fatalf("status = %s", g.Status)
	}
	n := pending(t, st)
	if len(n) != 1 || !strings.Contains(n[0].Body, "用 acmectl backup restore 指定快照還原") || !strings.Contains(n[0].Body, "FAQ ·") {
		t.Fatalf("notices = %+v", n)
	}

	// 來源 sha 變了 ⇒ 卡片過期
	marked, err := st.MarkStaleFAQs(ctx, "acme/handbook", map[string]string{"backup.md": "sha-new"})
	if err != nil || len(marked) != 1 {
		t.Fatalf("marked = %v %v", marked, err)
	}
	stale, err := st.ListStaleFAQs(ctx, map[string]map[string]string{"acme/handbook": {"backup.md": "sha-new"}})
	if err != nil || len(stale) != 1 || len(stale[0].Changed) != 1 {
		t.Fatalf("stale = %+v %v", stale, err)
	}
	// 已經過期的卡不重複標記
	if marked, _ := st.MarkStaleFAQs(ctx, "acme/handbook", map[string]string{"backup.md": "sha-new"}); len(marked) != 0 {
		t.Fatalf("already-stale card marked again: %v", marked)
	}
}

func TestCloseNotifiesOnlyWhenGatePasses(t *testing.T) {
	svc, st := setup(t)
	ctx := context.Background()
	g := ask(t, svc, "t1", "憑證怎麼換")
	if notified, err := svc.Close(ctx, g.ID); err != nil || notified {
		t.Fatalf("close without doc: notified=%v err=%v", notified, err)
	}
	g2 := ask(t, svc, "t2", "備份多久跑一次")
	addDoc(t, st, "acme/handbook", "backup.md", "備份每天凌晨兩點執行。")
	if notified, err := svc.Close(ctx, g2.ID); err != nil || !notified {
		t.Fatalf("close with doc: notified=%v err=%v", notified, err)
	}
	if err := svc.NeedsHuman(ctx, g.ID, ""); err == nil {
		t.Fatal("needs-human without note should fail")
	}
}
