package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// 需要 FOOTNOTE_TEST_DATABASE_URL（見 internal/gap 的整合測試）；沒設就略過。
func TestTryLockIsExclusive(t *testing.T) {
	url := os.Getenv("FOOTNOTE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FOOTNOTE_TEST_DATABASE_URL 未設定，略過整合測試")
	}
	ctx := context.Background()
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	release, ok, err := st.TryLock(ctx, 42)
	if err != nil || !ok {
		t.Fatalf("first lock: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.TryLock(ctx, 42); err != nil || ok {
		t.Fatalf("second lock should fail while held: ok=%v err=%v", ok, err)
	}
	release()
	release2, ok, err := st.TryLock(ctx, 42)
	if err != nil || !ok {
		t.Fatalf("lock after release: ok=%v err=%v", ok, err)
	}
	release2()
}

// 模型用量寫進 asks、每月預算從這裡加總。
func TestGenUsageSince(t *testing.T) {
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
	if err := Migrate(ctx, url, 4); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	base := Ask{Platform: "web", ChannelRef: "c", ThreadRef: "t", UserRef: "u", Question: "q", Query: "q", Status: "answer"}
	for _, a := range []Ask{
		{GenModel: "claude-haiku-5-5", InputTokens: 100, OutputTokens: 10},
		{GenModel: "claude-haiku-5-5", InputTokens: 50, OutputTokens: 5},
		{GenModel: "claude-opus-5-5", InputTokens: 7, OutputTokens: 1},
		{}, // 沒呼叫模型的提問不算
		// 翻譯補查的用量一起算：同一則提問補查加整理回答兩次呼叫，以及只補查沒整理的
		{GenModel: "claude-haiku-5-5", InputTokens: 1000, OutputTokens: 100, RescueModel: "claude-haiku-5-5", RescueInputTokens: 300, RescueOutputTokens: 40},
		{RescueModel: "claude-haiku-5-5", RescueInputTokens: 200, RescueOutputTokens: 30},
	} {
		x := base
		x.GenModel, x.InputTokens, x.OutputTokens = a.GenModel, a.InputTokens, a.OutputTokens
		x.RescueModel, x.RescueInputTokens, x.RescueOutputTokens = a.RescueModel, a.RescueInputTokens, a.RescueOutputTokens
		if _, err := st.InsertAsk(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.GenUsageSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["claude-haiku-5-5"] != [2]int64{150 + 1000 + 300 + 200, 15 + 100 + 40 + 30} || got["claude-opus-5-5"] != [2]int64{7, 1} {
		t.Fatalf("usage = %v", got)
	}
	if got, _ := st.GenUsageSince(ctx, time.Now().Add(time.Hour)); len(got) != 0 {
		t.Fatalf("future since should be empty: %v", got)
	}
}

// footnote grep 的資料來源：照字面找、不分大小寫，% 不是萬用字元；Mentions 再照詞彙表的規則篩行。
func TestChunksContaining(t *testing.T) {
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
	if err := Migrate(ctx, url, 4); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	f := FileMeta{ProjectPath: "p", Path: "doc.xlsx", Ref: "main", BlobSHA: "s", Kind: "doc"}
	vec := []float32{1, 0, 0, 0}
	if err := st.ReplaceFile(ctx, f, []ChunkRow{
		{Heading: "a", LineStart: 1, LineEnd: 1, Content: "Buy the Season Pass", Embedding: vec, TSV: "x"},
		{Heading: "b", LineStart: 2, LineEnd: 2, Content: "百分比 100%", Embedding: vec, TSV: "x"},
		{Heading: "c", LineStart: 3, LineEnd: 3, Content: "別的", Embedding: vec, TSV: "x"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	for word, want := range map[string]string{"season pass": "a", "SEASON": "a", "100%": "b"} {
		got, err := st.ChunksContaining(ctx, word)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Heading != want {
			t.Errorf("%q 得到 %+v", word, got)
		}
	}
	if got, err := st.ChunksContaining(ctx, "%"); err != nil || len(got) != 1 {
		t.Errorf("百分號要照字面找：%+v %v", got, err)
	}
	if got, err := st.ChunksContaining(ctx, "沒有這個字"); err != nil || len(got) != 0 {
		t.Errorf("找不到時是空的：%+v %v", got, err)
	}
}
