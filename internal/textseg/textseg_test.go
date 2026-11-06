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
		{"回滾流程", []string{"回滾", "滾流", "流程"}},
		{"單", []string{"單"}},
		{"Disk Io Bar", []string{"disk", "io", "bar"}},
		{"快速部署v2功能", []string{"快速", "速部", "部署", "v2", "功能"}},
		{"LimitTimeGiftPack_Special", []string{"limittimegiftpack", "special"}},
		{"費用（CP）= 流量×1.1", []string{"費用", "cp", "流量", "1", "1"}},
		{"", nil},
	}
	for _, c := range cases {
		if got := Tokens(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Tokens(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQueryDedupes(t *testing.T) {
	if got, want := Query("回滾 回滾 rollback"), "回滾 or rollback"; got != want {
		t.Errorf("Query = %q, want %q", got, want)
	}
}
