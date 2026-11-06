package glossary

import (
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	groups, err := Parse([]byte("# 註解\n- [回滾, rollback]\n- [ 值班輪替 , 輪值表 ]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0][1] != "rollback" || groups[1][0] != "值班輪替" {
		t.Fatalf("groups = %q", groups)
	}
	if g, err := Parse(nil); err != nil || len(g) != 0 {
		t.Fatalf("empty file: %q, %v", g, err)
	}
	if _, err := Parse([]byte("回滾: rollback\n")); err == nil {
		t.Fatal("mapping at top level should fail")
	}
}

func TestLint(t *testing.T) {
	if probs := Lint([][]string{{"回滾", "rollback"}, {"值班輪替", "輪值表", "oncall"}}); len(probs) != 0 {
		t.Fatalf("valid glossary: %q", probs)
	}
	for _, tc := range []struct {
		groups [][]string
		want   string
	}{
		{[][]string{{"回滾"}}, "至少要兩個叫法"},
		{[][]string{{"回滾", ""}}, "空的叫法"},
		{[][]string{{"淵", "rollback"}}, "太短"},
		{[][]string{{"回滾", "rollback", "Rollback"}}, "重複"},
		// 同一個叫法屬於兩組：補上去會把兩件事混在一起
		{[][]string{{"回滾", "rollback"}, {"ROLLBACK", "回滾作業"}}, "只能屬於一組"},
	} {
		probs := Lint(tc.groups)
		if len(probs) == 0 || !strings.Contains(strings.Join(probs, "\n"), tc.want) {
			t.Errorf("Lint(%q) = %q, want %q", tc.groups, probs, tc.want)
		}
	}
}

func TestExpand(t *testing.T) {
	g := New([][]string{{"回滾", "rollback"}, {"值班輪替", "輪值表"}, {"團隊", "小組", "team"}})
	for _, tc := range []struct{ in, want string }{
		// 別名補上文件的正式名稱，補在第一個出現的別名後面
		{"rollback 的權限有哪些？", "rollback（回滾） 的權限有哪些？"},
		// 只有這個方向：已經用正式名稱問的問題不補別名，補了只會拉低分數
		{"回滾的權限", "回滾的權限"},
		{"團隊有幾個人", "團隊有幾個人"},
		// 一組有好幾個別名：只補正式名稱，不補其他別名
		{"team 有幾個人", "team（團隊） 有幾個人"},
		{"小組跟 team 一樣嗎", "小組（團隊）跟 team 一樣嗎"},
		// 不分大小寫；前後是中文字也算詞的邊界
		{"Rollback的權限", "Rollback（回滾）的權限"},
		{"PROJ-ROLLBACK 做完了嗎", "PROJ-ROLLBACK（回滾） 做完了嗎"},
		// 英文叫法要整個詞對上
		{"rollbacks 是什麼", "rollbacks 是什麼"},
		{"myrollback 是什麼", "myrollback 是什麼"},
		// 問題裡已經有正式名稱就不補；再展開一次結果不變
		{"rollback（回滾）的權限", "rollback（回滾）的權限"},
		// 多組各補各的；討論串的多行字串也照補
		{"rollback 要多久？\n輪值表多久換一次？", "rollback（回滾） 要多久？\n輪值表（值班輪替）多久換一次？"},
		// 沒出現就原樣
		{"憑證可以重複申請嗎？", "憑證可以重複申請嗎？"},
	} {
		if got := g.Expand(tc.in); got != tc.want {
			t.Errorf("Expand(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := g.Expand(g.Expand(tc.in)); got != tc.want {
			t.Errorf("Expand twice (%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// 同一個詞前面有不算數的出現（rollbacks），後面那個才算
	if got := g.Expand("rollbacks 跟 rollback 一樣嗎"); got != "rollbacks 跟 rollback（回滾） 一樣嗎" {
		t.Errorf("later whole-word match: %q", got)
	}
	// 沒有詞彙表：原樣回傳
	var none *Glossary
	if got := none.Expand("rollback"); got != "rollback" {
		t.Errorf("nil glossary: %q", got)
	}
	if got := New(nil).Expand("rollback"); got != "rollback" {
		t.Errorf("empty glossary: %q", got)
	}
}

func TestCompare(t *testing.T) {
	before := [][]string{{"回滾", "rollback"}, {"金絲雀", "canary"}}
	cases := []struct {
		name    string
		after   [][]string
		added   []string
		isNew   bool
		wantErr string
	}{
		{name: "新的一組不算第一個", after: [][]string{{"回滾", "rollback"}, {"金絲雀", "canary"}, {"憑證", "cert"}}, added: []string{"cert"}, isNew: true},
		{name: "既有的一組加叫法", after: [][]string{{"回滾", "rollback", "倒回"}, {"金絲雀", "canary"}}, added: []string{"倒回"}},
		{name: "新的一組有兩個別名", after: [][]string{{"回滾", "rollback"}, {"金絲雀", "canary"}, {"團隊", "小組", "team"}}, added: []string{"小組", "team"}, isNew: true},
		{name: "大小寫不同不算新叫法", after: [][]string{{"回滾", "ROLLBACK"}, {"金絲雀", "canary"}}, wantErr: "沒有新增"},
		{name: "沒改", after: before, wantErr: "沒有新增"},
		{name: "拿掉叫法要人審", after: [][]string{{"回滾", "rollback"}, {"金絲雀", "灰度"}}, wantErr: "不能拿掉"},
		{name: "線上多出來的叫法提案裡沒有", after: [][]string{{"回滾", "rollback"}, {"憑證", "cert"}}, wantErr: "不能拿掉"},
		{name: "一次改兩組", after: [][]string{{"回滾", "rollback", "倒回"}, {"金絲雀", "canary", "灰度"}}, wantErr: "只能改一組"},
		{name: "併組", after: [][]string{{"回滾", "rollback", "金絲雀", "canary"}}, wantErr: "併在一起"},
		{name: "拆組", after: [][]string{{"回滾"}, {"rollback"}, {"金絲雀", "canary"}}, wantErr: "拆開"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch, err := Compare(before, c.after)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("要回傳含「%s」的錯誤，得到 %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(ch.Added, c.added) || ch.New != c.isNew {
				t.Fatalf("得到 %+v", ch)
			}
		})
	}
}

func TestMentions(t *testing.T) {
	cases := []struct {
		s, name string
		want    bool
	}{
		{"Cert 上限", "cert", true},
		{"certes 上限", "cert", false},
		{"rollback的權限", "rollback", true},
		{"回滾的權限", "回滾", true},
		{"沒有提到", "回滾", false},
		{"什麼都行", " ", false},
	}
	for _, c := range cases {
		if got := Mentions(c.s, c.name); got != c.want {
			t.Errorf("Mentions(%q, %q) = %v", c.s, c.name, got)
		}
	}
}
