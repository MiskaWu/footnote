package source

import (
	"context"
	"testing"

	"github.com/miskawu/footnote/internal/config"
)

type fakeDocs map[string]string

func (f fakeDocs) KBCurrent(context.Context) (map[string]string, error) { return f, nil }

// sha 要跟 git blob 的定義一樣：從目錄匯入的內容跟原本入庫的 sha 相同，才不會全部重新嵌入。
func TestDBListsWithBlobSHA(t *testing.T) {
	ctx := context.Background()
	d := NewDB(config.Source{Name: "faq", ProjectPath: "kb/faq"}, fakeDocs{"a/b.md": "hello\n", "glossary.yaml": "- [x, y]\n"})
	files, err := d.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if files["a/b.md"] != "ce013625030ba8dba906f756967f9e9ca394464a" { // git hash-object 算出來的 hello\n
		t.Fatalf("sha = %s", files["a/b.md"])
	}
	sel := d.Selected(files)
	if len(sel) != 2 || sel["glossary.yaml"] == "" {
		t.Fatalf("selected = %v", sel)
	}
	if b, err := d.Read(ctx, "a/b.md"); err != nil || string(b) != "hello\n" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if d.IsDraft("a/b.md") {
		t.Fatal("FAQ 卡不是未確認設計")
	}
}
