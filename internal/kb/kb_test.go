package kb

import (
	"strings"
	"testing"
)

func TestValidPath(t *testing.T) {
	for _, p := range []string{"rollback/entry.md", "oncall/shift-cap.md", "glossary.yaml"} {
		if err := ValidPath(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range []string{"entry.md", "Rollback/entry.md", "a/b/c.md", "../x.md", "a/b.txt", "a/回滾.md", ""} {
		if ValidPath(p) == nil {
			t.Errorf("%q 應該不合法", p)
		}
	}
}

func TestDiff(t *testing.T) {
	got := Diff("一\n二\n三\n", "一\n貳\n三\n四\n")
	if got != "- 二\n+ 貳\n+ 四" {
		t.Fatalf("diff = %q", got)
	}
	if Diff("a\n", "a\n") != "" {
		t.Fatal("沒變就是空的")
	}
	if !strings.Contains(Diff("", "新"), "+ 新") {
		t.Fatal("全新的內容")
	}
}

func TestUndo(t *testing.T) {
	const before = "# 註解\n- [回滾, rollback]\n"
	cases := []struct {
		name, cur, after, want string
	}{
		{"拿掉新的一組與它的註解", "# 註解\n- [回滾, rollback]\n# 憑證\n- [憑證, cert]\n", "# 註解\n- [回滾, rollback]\n# 憑證\n- [憑證, cert]\n", before},
		{"之後加的留著", "# 註解\n- [回滾, rollback]\n- [憑證, cert]\n- [金絲雀, canary]\n", "# 註解\n- [回滾, rollback]\n- [憑證, cert]\n", "# 註解\n- [回滾, rollback]\n- [金絲雀, canary]\n"},
		{"被換掉的行放回原位", "# 註解\n- [回滾, rollback, 倒回]\n- [金絲雀, canary]\n", "# 註解\n- [回滾, rollback, 倒回]\n", "# 註解\n- [回滾, rollback]\n- [金絲雀, canary]\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := undo(c.cur, before, c.after)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("得到 %q，要 %q", got, c.want)
			}
		})
	}
	if _, err := undo(before, before, "# 註解\n- [回滾, rollback]\n- [憑證, cert]\n"); err == nil {
		t.Fatal("那行已經不在線上（撤回過）要報錯")
	}
}
