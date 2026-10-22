package tei

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TEI 單次最多收 32 筆；超過要分批送，分數要對回原本的順序。
func TestRerankBatchesOver32(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Texts []string }
		json.NewDecoder(r.Body).Decode(&req)
		calls++
		if len(req.Texts) > 32 {
			http.Error(w, `{"error":"batch size > 32"}`, http.StatusRequestEntityTooLarge)
			return
		}
		type res struct {
			Index int     `json:"index"`
			Score float32 `json:"score"`
		}
		var out []res
		for i := len(req.Texts) - 1; i >= 0; i-- { // 刻意倒序回傳
			var n int
			json.Unmarshal([]byte(req.Texts[i]), &n)
			out = append(out, res{Index: i, Score: float32(n)})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	c := New("", srv.URL, "", 0)
	docs := make([]string, 70)
	for i := range docs {
		b, _ := json.Marshal(i)
		docs[i] = string(b)
	}
	scores, err := c.Rerank(context.Background(), "q", docs)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	for i, s := range scores {
		if int(s) != i {
			t.Fatalf("scores[%d] = %v", i, s)
		}
	}
}
