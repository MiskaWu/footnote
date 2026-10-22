package textseg

import (
	"reflect"
	"testing"
)

func TestTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"部署流程", []string{"部署", "署流", "流程"}},
		{"單", []string{"單"}},
		{"Api Rate Limit", []string{"api", "rate", "limit"}},
		{"一鍵部署v2功能", []string{"一鍵", "鍵部", "部署", "v2", "功能"}},
		{"ReleaseNotesPanel_Special", []string{"releasenotespanel", "special"}},
		{"延遲（P95）= 基準×1.1", []string{"延遲", "p95", "基準", "1", "1"}},
		{"", nil},
	}
	for _, c := range cases {
		if got := Tokens(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Tokens(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQueryDedupes(t *testing.T) {
	if got, want := Query("部署 部署 deploy"), "部署 or deploy"; got != want {
		t.Errorf("Query = %q, want %q", got, want)
	}
}
