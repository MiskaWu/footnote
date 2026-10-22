package qa

import (
	"context"
	"strings"
	"testing"

	"github.com/miskawu/footnote/internal/config"
	"github.com/miskawu/footnote/internal/store"
	"github.com/miskawu/footnote/internal/tei"
)

var cfg = config.QA{Candidates: 30, KReply: 3, TauAnswer: 0.35, TauKeep: 0.15}

func hit(id int64, kind string, score float32) Hit {
	return Hit{Candidate: store.Candidate{ID: id, Kind: kind, ProjectPath: "p", Path: "f.md"}, Score: score}
}

func TestDecideGateABlocks(t *testing.T) {
	r := Decide([]Hit{hit(1, "doc", 0.34), hit(2, "doc", 0.2)}, cfg)
	if r.Status != StatusNoData || r.Reason != "gate_a" || len(r.Hits) != 0 {
		t.Fatalf("got %+v", r)
	}
	if len(r.NearMisses) != 2 {
		t.Errorf("near misses = %d", len(r.NearMisses))
	}
}

func TestDecideEmpty(t *testing.T) {
	if r := Decide(nil, cfg); r.Status != StatusNoData {
		t.Fatalf("got %+v", r)
	}
}

func TestDecideDocsRespectsTauKeepAndKReply(t *testing.T) {
	r := Decide([]Hit{hit(1, "doc", 0.9), hit(2, "doc", 0.5), hit(3, "doc", 0.14), hit(4, "doc", 0.13)}, cfg)
	if r.Status != StatusDocs || len(r.Hits) != 2 {
		t.Fatalf("got status=%s hits=%d", r.Status, len(r.Hits))
	}
	r = Decide([]Hit{hit(1, "doc", 0.9), hit(2, "doc", 0.8), hit(3, "doc", 0.7), hit(4, "doc", 0.6)}, cfg)
	if len(r.Hits) != 3 {
		t.Fatalf("k_reply not applied: %d", len(r.Hits))
	}
}

func TestDecideFAQOnTop(t *testing.T) {
	r := Decide([]Hit{hit(1, "faq", 0.8), hit(2, "doc", 0.7)}, cfg)
	if r.Status != StatusFAQ || len(r.Hits) != 1 || r.Hits[0].ID != 1 {
		t.Fatalf("got %+v", r)
	}
}

func TestDecideFAQNotMixedIntoDocs(t *testing.T) {
	r := Decide([]Hit{hit(1, "doc", 0.8), hit(2, "faq", 0.7), hit(3, "doc", 0.6)}, cfg)
	if r.Status != StatusDocs || len(r.Hits) != 2 || r.Hits[1].ID != 3 {
		t.Fatalf("got %+v", r.Hits)
	}
}

// ---- Engine 用假實作 ----

type fakeStore struct {
	cands []store.Candidate
	faq   *store.FAQ
}

func (f *fakeStore) HybridSearch(ctx context.Context, vec []float32, fts string, k, limit int, prefixes []string) ([]store.Candidate, error) {
	return f.cands, nil
}
func (f *fakeStore) GetFAQ(ctx context.Context, project, path string) (*store.FAQ, error) {
	if f.faq == nil {
		return nil, store.ErrNotFound
	}
	return f.faq, nil
}

type fakeEmbed struct{ got []string }

func (f *fakeEmbed) Embed(ctx context.Context, texts []string, kind tei.EmbedKind) ([][]float32, error) {
	f.got = append(f.got, texts...)
	return [][]float32{{1, 0}}, nil
}

type fakeRerank map[string]float32 // content → score

func (f fakeRerank) Rerank(ctx context.Context, query string, docs []string) ([]float32, error) {
	out := make([]float32, len(docs))
	for i, d := range docs {
		for k, v := range f {
			if strings.HasSuffix(d, k) {
				out[i] = v
			}
		}
	}
	return out, nil
}

func TestEngineRerankOrdersAndFetchesFAQ(t *testing.T) {
	st := &fakeStore{
		cands: []store.Candidate{
			{ID: 1, Kind: "doc", Content: "低分"},
			{ID: 2, Kind: "faq", Content: "高分卡片", ProjectPath: "acme/faq", Path: "a.md"},
		},
		faq: &store.FAQ{Path: "a.md"},
	}
	emb := &fakeEmbed{}
	e := &Engine{Store: st, Embedder: emb, Reranker: fakeRerank{"低分": 0.4, "高分卡片": 0.9}, Cfg: cfg}
	r, err := e.Run(context.Background(), "怎麼回滾", []string{"部署流程是什麼"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusFAQ || r.Hits[0].ID != 2 || r.FAQ == nil {
		t.Fatalf("got %+v", r)
	}
	if emb.got[0] != "部署流程是什麼\n怎麼回滾" {
		t.Errorf("follow-up query = %q", emb.got[0])
	}
}

func TestWindows(t *testing.T) {
	content := "a1\na2\na3\na4\na5\na6\na7"
	ws := Windows(content, 100, 4)
	if len(ws) != 3 || ws[0].Text != "a1\na2\na3\na4" || ws[1].Text != "a3\na4\na5\na6" || ws[2].Text != "a5\na6\na7" {
		t.Fatalf("windows = %+v", ws)
	}
	if !ws[0].First || ws[0].Last || !ws[2].Last {
		t.Errorf("first/last flags wrong: %+v", ws)
	}
	if got := ws[1].Display(); got != "…\na3\na4\na5\na6\n…" {
		t.Errorf("display = %q", got)
	}
	// 字數上限：第一行就超過時截斷該行
	long := Windows(strings.Repeat("長", 50)+"\n短", 10, 4)
	if long[0].Text != strings.Repeat("長", 10)+"…" {
		t.Errorf("long line = %q", long[0].Text)
	}
}

// 重排只看窗口有沒有「7D」，確認挑的是含答案的那組行，而不是片段開頭。
type answerRerank struct{}

func (answerRerank) Rerank(ctx context.Context, q string, docs []string) ([]float32, error) {
	out := make([]float32, len(docs))
	for i, d := range docs {
		out[i] = 0.9 // 片段本身
		if strings.Contains(d, "7D") {
			out[i] = 0.99
		} else if !strings.Contains(d, "日誌") {
			out[i] = 0.1
		}
	}
	return out, nil
}

func TestRunPicksExcerptWithAnswer(t *testing.T) {
	content := "日誌保留的計算方式\n階段1-1\n階段1-2\n階段1-3\n階段1-4\n階段1-5\n日誌保留天數上限\n基礎方案 | 7D\n進階方案 | 基礎再加上30D"
	st := &fakeStore{cands: []store.Candidate{{ID: 1, Kind: "doc", Content: content}}}
	c := cfg
	c.ExcerptChars = 200
	e := &Engine{Store: st, Embedder: &fakeEmbed{}, Reranker: answerRerank{}, Cfg: c}
	r, err := e.Run(context.Background(), "日誌最多保留幾天", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusDocs || !strings.Contains(r.Hits[0].Excerpt, "基礎方案 | 7D") || strings.Contains(r.Hits[0].Excerpt, "階段1-1") {
		t.Fatalf("excerpt = %q", r.Hits[0].Excerpt)
	}
}
