package kb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/miskawu/footnote/internal/ingest"
	"github.com/miskawu/footnote/internal/store"
)

const card = `---
questions: [怎麼部署？, 部署流程是什麼]
sources: ["team/docs:guide.md#部署"]
verified: 2026-11-06
---
先跑測試、打 tag [1]。
`

// 用假的檢查：內容含 BAD 就不通過。真的檢查規則在 internal/faq、internal/glossary 測。
func service(t *testing.T) (*Service, context.Context, *int) {
	t.Helper()
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
	kicks := 0
	return &Service{
		Store: st, Reviewers: []string{"alice"}, Kick: func() { kicks++ },
		Lint: func(context.Context) (ingest.LintFunc, error) {
			return func(_ context.Context, _ string, data []byte) ([]string, error) {
				if strings.Contains(string(data), "BAD") {
					return []string{"內容有問題"}, nil
				}
				return nil, nil
			}, nil
		},
	}, ctx, &kicks
}

func current(t *testing.T, s *Service, ctx context.Context) map[string]string {
	t.Helper()
	cur, err := s.Store.KBCurrent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return cur
}

func TestProposeChecksFirst(t *testing.T) {
	s, ctx, _ := service(t)
	var lint *LintError
	if _, err := s.Propose(ctx, "a/b.md", []byte("BAD"), "n", "agent"); !errors.As(err, &lint) {
		t.Fatalf("沒過檢查的內容不能進待審：%v", err)
	}
	if _, err := s.Propose(ctx, "bad-path.md", []byte(card), "n", "agent"); err == nil {
		t.Fatal("路徑不對要擋")
	}
	id, err := s.Propose(ctx, "a/b.md", []byte(card), "n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(current(t, s, ctx)) != 0 {
		t.Fatal("提案不等於上線")
	}
	if err := s.Approve(ctx, id, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(ctx, "a/b.md", []byte(card), "n", "agent"); err == nil {
		t.Fatal("內容跟線上一樣不用提案")
	}
}

// 這條是整個設計的防線：代理能提案，但只有名單上的人能核准；不在名單上的人按了不會有任何效果。
func TestOnlyReviewersDecide(t *testing.T) {
	s, ctx, kicks := service(t)
	id, err := s.Propose(ctx, "a/b.md", []byte(card), "n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"agent", "bob", ""} {
		if err := s.Approve(ctx, id, user); !errors.Is(err, ErrNotReviewer) {
			t.Fatalf("%q 不該能核准：%v", user, err)
		}
		if err := s.Reject(ctx, id, user, "x"); !errors.Is(err, ErrNotReviewer) {
			t.Fatalf("%q 不該能退回：%v", user, err)
		}
	}
	if c, _ := s.Store.KBGet(ctx, id); c.Status != "pending" || len(current(t, s, ctx)) != 0 || *kicks != 0 {
		t.Fatalf("狀態被動到了：%+v", c)
	}
	if err := s.Approve(ctx, id, "alice"); err != nil {
		t.Fatal(err)
	}
	if cur := current(t, s, ctx); cur["a/b.md"] != card {
		t.Fatalf("current = %v", cur)
	}
	if *kicks != 1 {
		t.Fatalf("核准要馬上叫同步，kicks = %d", *kicks)
	}
	if c, _ := s.Store.KBGet(ctx, id); c.Reviewer != "alice" || c.ReviewedAt == nil {
		t.Fatalf("要留下誰核准的：%+v", c)
	}
	if err := s.Approve(ctx, id, "alice"); !errors.Is(err, ErrDecided) {
		t.Fatalf("不能審第二次：%v", err)
	}
}

// 來源可能在提案之後被刪掉：核准時再檢查一次，讓審核的人當場知道，提案維持待審。
func TestApproveRechecks(t *testing.T) {
	s, ctx, kicks := service(t)
	id, err := s.Propose(ctx, "a/b.md", []byte(card), "n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	s.Lint = func(context.Context) (ingest.LintFunc, error) {
		return func(context.Context, string, []byte) ([]string, error) { return []string{"來源不見了"}, nil }, nil
	}
	var lint *LintError
	if err := s.Approve(ctx, id, "alice"); !errors.As(err, &lint) {
		t.Fatalf("err = %v", err)
	}
	if c, _ := s.Store.KBGet(ctx, id); c.Status != "pending" || *kicks != 0 {
		t.Fatalf("change = %+v", c)
	}
}

func TestNewProposalSupersedes(t *testing.T) {
	s, ctx, _ := service(t)
	first, _ := s.Propose(ctx, "a/b.md", []byte(card), "第一版", "agent")
	second, err := s.Propose(ctx, "a/b.md", []byte(card+"補充 [1]。\n"), "第二版", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(ctx, first, "alice"); !errors.Is(err, ErrDecided) {
		t.Fatalf("被取代的提案不能核准：%v", err)
	}
	if err := s.Approve(ctx, second, "alice"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(current(t, s, ctx)["a/b.md"], "補充") {
		t.Fatal("要是第二版")
	}
	list, _ := s.Store.KBList(ctx, "", 10)
	if len(list) != 2 || list[1].Status != "superseded" {
		t.Fatalf("修改紀錄都要留著：%+v", list)
	}
}

func TestRejectKeepsReason(t *testing.T) {
	s, ctx, kicks := service(t)
	id, _ := s.Propose(ctx, "a/b.md", []byte(card), "n", "agent")
	if err := s.Reject(ctx, id, "alice", "第二句來源不對"); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Store.KBGet(ctx, id)
	if c.Status != "rejected" || c.ReviewNote != "第二句來源不對" || *kicks != 0 || len(current(t, s, ctx)) != 0 {
		t.Fatalf("change = %+v", c)
	}
	// 退回後可以重提
	if _, err := s.Propose(ctx, "a/b.md", []byte(card), "改好了", "agent"); err != nil {
		t.Fatal(err)
	}
}

func TestRetire(t *testing.T) {
	s, ctx, _ := service(t)
	if _, err := s.Retire(ctx, "a/b.md", "n", "agent"); err == nil {
		t.Fatal("線上沒有的卡不能刪")
	}
	id, _ := s.Propose(ctx, "a/b.md", []byte(card), "n", "agent")
	s.Approve(ctx, id, "alice")
	if _, err := s.Retire(ctx, "a/b.md", " ", "agent"); err == nil {
		t.Fatal("刪除要寫原因")
	}
	id, err := s.Retire(ctx, "a/b.md", "功能拿掉了", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(current(t, s, ctx)) != 1 {
		t.Fatal("核准前還在")
	}
	if err := s.Approve(ctx, id, "alice"); err != nil {
		t.Fatal(err)
	}
	if len(current(t, s, ctx)) != 0 {
		t.Fatal("核准後要不見")
	}
	if list, _ := s.Store.KBList(ctx, "approved", 10); len(list) != 2 {
		t.Fatalf("歷史要留著：%+v", list)
	}
}

// 匯入只能做一次：不然代理可以拿匯入繞過審核。
func TestImportOnlyOnce(t *testing.T) {
	s, ctx, _ := service(t)
	dir := t.TempDir()
	for p, c := range map[string]string{
		"rollback/entry.md": card, "glossary.yaml": "- [回滾, rollback]\n", "README.md": "# 不是卡片", "CLAUDE.md": "# 也不是",
		".claude/commands/handle-gaps.md": "# 不是卡片",
	} {
		f := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Import(ctx, dir, "alice")
	if err != nil || n != 2 {
		t.Fatalf("n = %d, err = %v", n, err)
	}
	cur := current(t, s, ctx)
	if cur["rollback/entry.md"] != card || cur["glossary.yaml"] == "" || len(cur) != 2 {
		t.Fatalf("current = %v", cur)
	}
	if _, err := s.Import(ctx, dir, "alice"); err == nil {
		t.Fatal("第二次要拒絕")
	}
}

func TestPreview(t *testing.T) {
	s, ctx, _ := service(t)
	id, _ := s.Propose(ctx, "a/b.md", []byte(card), "回答 G3", "agent")
	c, _ := s.Store.KBGet(ctx, id)
	p, err := s.BuildPreview(ctx, *c)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "新增 a/b.md" || !strings.Contains(p.Body, "先跑測試") || p.Diff != "" || len(p.Lines) != 2 {
		t.Fatalf("新增：%+v", p)
	}
	s.Approve(ctx, id, "alice")

	id, _ = s.Propose(ctx, "a/b.md", []byte(strings.Replace(card, "打 tag", "打版本號", 1)), "n", "agent")
	c, _ = s.Store.KBGet(ctx, id)
	if p, _ = s.BuildPreview(ctx, *c); p.Title != "修改 a/b.md" || !strings.Contains(p.Diff, "- 先跑測試、打 tag") || !strings.Contains(p.Diff, "+ 先跑測試、打版本號") {
		t.Fatalf("修改：%+v", p)
	}
	s.Approve(ctx, id, "alice")

	id, _ = s.Retire(ctx, "a/b.md", "拿掉", "agent")
	c, _ = s.Store.KBGet(ctx, id)
	if p, _ = s.BuildPreview(ctx, *c); p.Title != "刪除 a/b.md" || !strings.Contains(p.Body, "打版本號") {
		t.Fatalf("刪除要看得到被刪的內容：%+v", p)
	}
}

const (
	glossary1 = "- [回滾, rollback]\n"
	glossary2 = "- [回滾, rollback]\n# 憑證\n- [憑證, cert]\n"
	glossary3 = "- [回滾, rollback]\n# 憑證\n- [憑證, cert]\n- [金絲雀, canary]\n"
)

// passes 是假的程式檢查：一律通過並留下紀錄。真的檢查在 eval.CheckAlias 測。
func passes(context.Context, *GlossaryProposal) (string, error) { return "問 0.1 ⇒ 0.8", nil }

// onlineGlossary 讓線上先有一份詞彙表。
func onlineGlossary(t *testing.T, s *Service, ctx context.Context) {
	t.Helper()
	id, err := s.Propose(ctx, "glossary.yaml", []byte(glossary1), "n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(ctx, id, "alice"); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptGlossary(t *testing.T) {
	s, ctx, kicks := service(t)
	onlineGlossary(t, s, ctx)
	*kicks = 0
	id, err := s.ProposeGlossary(ctx, []byte(glossary2), "cert：提問 #50", "curator")
	if err != nil {
		t.Fatal(err)
	}
	unposted, err := s.Store.KBUnposted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unposted) != 0 {
		t.Fatalf("審核代理決定的提案不貼到 Discord：%+v", unposted)
	}
	if err := s.AcceptGlossary(ctx, id, func(context.Context, *GlossaryProposal) (string, error) {
		return "", errors.New("檢查沒通過")
	}); err == nil {
		t.Fatal("檢查沒過不能核准")
	}
	if got := current(t, s, ctx)["glossary.yaml"]; got != glossary1 {
		t.Fatalf("檢查沒過線上不能變：%q", got)
	}
	if err := s.AcceptGlossary(ctx, id, func(_ context.Context, p *GlossaryProposal) (string, error) {
		if len(p.Diff.Added) != 1 || p.Diff.Added[0] != "cert" {
			t.Fatalf("要驗的叫法：%+v", p.Diff)
		}
		return passes(ctx, p)
	}); err != nil {
		t.Fatal(err)
	}
	if got := current(t, s, ctx)["glossary.yaml"]; got != glossary2 {
		t.Fatalf("核准後要上線：%q", got)
	}
	if *kicks != 1 {
		t.Fatalf("核准後要叫同步，叫了 %d 次", *kicks)
	}
	c, err := s.Store.KBGet(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Reviewer != AgentReviewer || c.ReviewNote == "" {
		t.Fatalf("紀錄要看得出是審核代理、憑什麼核准：%+v", c)
	}
	if err := s.AcceptGlossary(ctx, id, passes); !errors.Is(err, ErrDecided) {
		t.Fatalf("審過的不能再審：%v", err)
	}
}

// FAQ 卡與走人審的提案都不能走審核代理這條路。
func TestAcceptGlossaryOnlyAgentProposals(t *testing.T) {
	s, ctx, _ := service(t)
	onlineGlossary(t, s, ctx)
	cardID, err := s.Propose(ctx, "a/b.md", []byte(card), "n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptGlossary(ctx, cardID, passes); !errors.Is(err, ErrNotAgentReview) {
		t.Fatalf("FAQ 卡不能由代理核准：%v", err)
	}
	if err := s.RejectGlossary(ctx, cardID, "不要"); !errors.Is(err, ErrNotAgentReview) {
		t.Fatalf("FAQ 卡不能由代理退回：%v", err)
	}
	humanID, err := s.Propose(ctx, "glossary.yaml", []byte(glossary2), "n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptGlossary(ctx, humanID, passes); !errors.Is(err, ErrNotAgentReview) {
		t.Fatalf("走人審的詞彙表提案不能由代理核准：%v", err)
	}
	if _, err := s.ProposeGlossary(ctx, []byte(glossary2), "n", "agent"); err == nil || !strings.Contains(err.Error(), "等人審") {
		t.Fatalf("不能用代理的提案取代還在等人審的提案：%v", err)
	}
}

func TestProposeGlossaryOneGroup(t *testing.T) {
	s, ctx, _ := service(t)
	onlineGlossary(t, s, ctx)
	for name, content := range map[string]string{
		"一次兩組": "- [回滾, rollback]\n- [憑證, cert]\n- [金絲雀, canary]\n",
		"拿掉叫法": "- [憑證, cert]\n",
		"沒有改":  glossary1,
	} {
		if _, err := s.ProposeGlossary(ctx, []byte(content), "n", "agent"); err == nil {
			t.Errorf("%s 要擋", name)
		}
	}
	var lint *LintError
	if _, err := s.ProposeGlossary(ctx, []byte("BAD"), "n", "agent"); !errors.As(err, &lint) {
		t.Fatalf("沒過檢查的內容不能進待審：%v", err)
	}
}

// 提案之後線上變了：整份內容核准下去會蓋掉別人的修改，要擋下來重提。
func TestAcceptGlossaryStaleBase(t *testing.T) {
	s, ctx, _ := service(t)
	onlineGlossary(t, s, ctx)
	id, err := s.ProposeGlossary(ctx, []byte(glossary2), "n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	// 撤回會寫新的線上內容，也會取代待審的提案
	if _, err := s.Store.KBRevert(ctx, store.KBChange{Path: "glossary.yaml", Content: ptr("- [回滾, rollback]\n- [金絲雀, canary]\n"), ProposedBy: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptGlossary(ctx, id, passes); !errors.Is(err, ErrDecided) {
		t.Fatalf("被取代的提案不能核准：%v", err)
	}
	// 待審的提案沒被取代、內容卻跟線上對不上
	stale, err := s.Store.KBPropose(ctx, store.KBChange{Path: "glossary.yaml", Content: ptr(glossary2), ProposedBy: "x", ReviewVia: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptGlossary(ctx, stale, passes); err == nil || !strings.Contains(err.Error(), "拿掉") {
		t.Fatalf("內容跟線上對不上要擋：%v", err)
	}
	if got := current(t, s, ctx)["glossary.yaml"]; !strings.Contains(got, "canary") {
		t.Fatalf("線上的修改不能被蓋掉：%q", got)
	}
}

func ptr(s string) *string { return &s }

func TestRejectGlossary(t *testing.T) {
	s, ctx, _ := service(t)
	onlineGlossary(t, s, ctx)
	id, err := s.ProposeGlossary(ctx, []byte(glossary2), "n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RejectGlossary(ctx, id, " "); err == nil {
		t.Fatal("退回要寫原因")
	}
	if err := s.RejectGlossary(ctx, id, "可能指兩件事"); err != nil {
		t.Fatal(err)
	}
	c, err := s.Store.KBGet(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "rejected" || c.ReviewNote != "可能指兩件事" {
		t.Fatalf("%+v", c)
	}
	if got := current(t, s, ctx)["glossary.yaml"]; got != glossary1 {
		t.Fatalf("退回不能動線上：%q", got)
	}
}

func TestRevertGlossary(t *testing.T) {
	s, ctx, kicks := service(t)
	onlineGlossary(t, s, ctx)
	first, err := s.ProposeGlossary(ctx, []byte(glossary2), "n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptGlossary(ctx, first, passes); err != nil {
		t.Fatal(err)
	}
	second, err := s.ProposeGlossary(ctx, []byte(glossary3), "n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptGlossary(ctx, second, passes); err != nil {
		t.Fatal(err)
	}
	*kicks = 0
	if _, err := s.RevertGlossary(ctx, first, "alice", "補錯了"); err != nil {
		t.Fatal(err)
	}
	if got, want := current(t, s, ctx)["glossary.yaml"], "- [回滾, rollback]\n- [金絲雀, canary]\n"; got != want {
		t.Fatalf("只拿掉那一筆加的，之後加的留著：%q", got)
	}
	if *kicks != 1 {
		t.Fatalf("撤回後要叫同步，叫了 %d 次", *kicks)
	}
	if _, err := s.RevertGlossary(ctx, first, "alice", ""); err == nil {
		t.Fatal("撤回過的不能再撤回")
	}
	list, err := s.Store.KBList(ctx, "approved", 1)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Reviewer != "revert" || !strings.Contains(list[0].Note, "撤回 #") {
		t.Fatalf("撤回要留紀錄：%+v", list[0])
	}
	cardID, err := s.Propose(ctx, "a/b.md", []byte(card), "n", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(ctx, cardID, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevertGlossary(ctx, cardID, "alice", ""); err == nil {
		t.Fatal("人核准的卡不能用這條路撤回")
	}
}
