package qa

import (
	"context"
	"strings"

	"github.com/miskawu/footnote/internal/llm"
)

// Generator 把段落整理成附出處的回答。
type Generator interface {
	Answer(ctx context.Context, question string, docs []llm.Doc) (*llm.Answer, error)
}

// Budget 決定這個月還能不能呼叫模型；不能時退回原文摘錄。
type Budget interface {
	Allow(ctx context.Context) (bool, error)
}

const defaultKMax = 6

// GenHits 挑送進生成的段落：文件段落、分數 ≥ τ_keep、最多 k_max 筆（FAQ 卡不混進來）。
func GenHits(ranked []Hit, tauKeep float64, kMax int) []Hit {
	if kMax <= 0 {
		kMax = defaultKMax
	}
	var out []Hit
	for _, h := range ranked {
		if len(out) >= kMax || float64(h.Score) < tauKeep {
			break
		}
		if h.Kind != "faq" {
			out = append(out, h)
		}
	}
	return out
}

// GenDocs 把段落轉成送給模型的文件，編號從 1 起，就是回答裡 [n] 的 n。
func GenDocs(hits []Hit) []llm.Doc {
	docs := make([]llm.Doc, len(hits))
	for i, h := range hits {
		docs[i] = llm.Doc{Index: i + 1, Source: Label(h.Candidate), Note: Note(h.Candidate), Text: h.Content}
	}
	return docs
}

// GenQuestion：討論串追問時，把前面的問題一起交給模型，它才知道「那」指什麼。
func GenQuestion(history []string, question string) string {
	if len(history) == 0 {
		return question
	}
	return "（同一討論串前面問過：" + strings.Join(history, "；") + "）\n" + question
}

// generate 在閘門 A 通過、回文件段落時呼叫模型。模型判斷沒有答案（閘門 B）或引用對不上（閘門 C）
// 就改成 no_data；API 錯誤、拒答、超出長度或預算用完，就保留原本的文件段落回覆（降級，不算缺口）。
// question 是 GenQuestion 組好、已展開別名的問題：模型才知道文件裡的「回滾」就是提問者說的 rollback。
func (e *Engine) generate(ctx context.Context, r *Result, question string) {
	if e.Budget != nil {
		ok, err := e.Budget.Allow(ctx)
		if err != nil || !ok {
			r.GenSkipped = "budget"
			e.warn("generation skipped", "reason", "budget", "err", err)
			return
		}
	}
	hits := GenHits(r.Ranked, e.Cfg.TauKeep, e.Cfg.KMax)
	if len(hits) == 0 {
		return
	}
	a, err := e.Gen.Answer(ctx, question, GenDocs(hits))
	if err != nil {
		r.GenSkipped = "error"
		e.warn("generation failed; falling back to excerpts", "err", err)
		return
	}
	r.Answer = a // 用量照記，預算要算
	switch {
	case a.Reason == "refusal" || a.Reason == "max_tokens":
		r.GenSkipped = a.Reason
		e.warn("generation unusable; falling back to excerpts", "reason", a.Reason)
	case a.Status == llm.StatusNoData:
		r.Status, r.Reason = StatusNoData, "gate_b"
		if a.Reason == "uncited" {
			r.Reason = "gate_c"
		}
		r.Hits = nil
	default:
		r.Status, r.Hits = StatusAnswer, hits
	}
}

func (e *Engine) warn(msg string, args ...any) {
	if e.Log != nil {
		e.Log.Warn(msg, args...)
	}
}
