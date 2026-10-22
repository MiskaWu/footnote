package eval

import (
	"testing"

	"github.com/miskawu/footnote/internal/qa"
	"github.com/miskawu/footnote/internal/store"
)

func TestMatches(t *testing.T) {
	h := qa.Hit{Candidate: store.Candidate{ProjectPath: "a/b", Path: "x.xlsx", Heading: "流程"}}
	if !matches([]string{"a/b:x.xlsx"}, h) || !matches([]string{"a/b:x.xlsx#流程"}, h) {
		t.Error("should match")
	}
	if matches([]string{"a/b:x.xlsx#別的"}, h) || matches([]string{"a/b:y.xlsx"}, h) {
		t.Error("should not match")
	}
}

func TestSummarizeAndPick(t *testing.T) {
	rs := []ItemResult{
		{Item: Item{Expect: "answer"}, Top: 0.9, Rank: 1},
		{Item: Item{Expect: "answer"}, Top: 0.4, Rank: 4},
		{Item: Item{Expect: "answer"}, Top: 0.2, Rank: 0},
		{Item: Item{Expect: "no_data"}, Top: 0.3},
		{Item: Item{Expect: "general"}, Top: 0.1},
	}
	s := Summarize(rs, 0.35, 3)
	if s.AnswerN != 3 || s.NegativeN != 2 || s.FalseRefusalN != 1 || s.MisleadingN != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if s.RecallAtK != 1.0/3 || s.MRR != (1+0.25)/3 {
		t.Errorf("recall=%v mrr=%v", s.RecallAtK, s.MRR)
	}
	// τ 0.35 與 0.40 都是零誤導、誤拒 1 題；0.30 以下會誤導、0.45 以上誤拒 2 題
	best := Pick(Sweep(rs, 3), 0)
	if best == nil || best.Tau < 0.349 || best.Tau > 0.351 {
		t.Errorf("pick = %+v", best)
	}
}

func TestPickTakesMiddleOfPlateau(t *testing.T) {
	rs := []ItemResult{
		{Item: Item{Expect: "answer"}, Top: 0.66},
		{Item: Item{Expect: "no_data"}, Top: 0.16},
	}
	best := Pick(Sweep(rs, 3), 0)
	if best == nil || best.Tau < 0.399 || best.Tau > 0.401 {
		t.Errorf("pick = %+v, want 0.40 (middle of 0.20..0.65)", best)
	}
}
