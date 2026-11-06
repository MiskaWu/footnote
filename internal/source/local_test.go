package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miskawu/footnote/internal/config"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// 設了 fetch：同步前抓遠端，讀 origin/<ref>；本機 checkout 的分支不動。
func TestFetchReadsOriginWithoutTouchingCheckout(t *testing.T) {
	up, work := t.TempDir(), filepath.Join(t.TempDir(), "work")
	git(t, up, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(up, "a.md"), []byte("v1\n"), 0o644)
	git(t, up, "add", "a.md")
	git(t, up, "commit", "-q", "-m", "v1")
	git(t, filepath.Dir(work), "clone", "-q", up, work)
	localMain := git(t, work, "rev-parse", "main")

	os.WriteFile(filepath.Join(up, "a.md"), []byte("v2\n"), 0o644)
	git(t, up, "commit", "-q", "-am", "v2")
	v2 := git(t, up, "rev-parse", "main:a.md")

	ctx := context.Background()
	l, err := NewLocal(ctx, config.Source{Name: "w", Dir: work, Ref: "main", Fetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := l.List(ctx); files["a.md"] == v2 {
		t.Fatal("should not see v2 before fetching")
	}
	if err := l.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	files, err := l.List(ctx)
	if err != nil || files["a.md"] != v2 {
		t.Fatalf("after fetch a.md = %s, want %s (err %v)", files["a.md"], v2, err)
	}
	if b, err := l.Read(ctx, "a.md"); err != nil || string(b) != "v2\n" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if got := git(t, work, "rev-parse", "main"); got != localMain {
		t.Errorf("local main moved: %s -> %s", localMain, got)
	}

	// 沒設 fetch：照舊讀本機的 main
	l2, _ := NewLocal(ctx, config.Source{Name: "w", Dir: work, Ref: "main"})
	if b, _ := l2.Read(ctx, "a.md"); string(b) != "v1\n" {
		t.Errorf("without fetch read = %q, want v1", b)
	}
}

// 設了 clone、資料夾還不存在：第一次自動 clone，讀得到 ref 的內容；之後不再重抓。
func TestCloneWhenMissing(t *testing.T) {
	up := t.TempDir()
	git(t, up, "init", "-q", "-b", "main")
	git(t, up, "config", "uploadpack.allowFilter", "true") // 讓 --filter=blob:none 真的生效（只抓目錄樹）
	os.WriteFile(filepath.Join(up, "a.md"), []byte("v1\n"), 0o644)
	git(t, up, "add", "a.md")
	git(t, up, "commit", "-q", "-m", "v1")

	dir := filepath.Join(t.TempDir(), "nested", "work")
	cfg := config.Source{Name: "w", Dir: dir, Ref: "main", Fetch: true, Clone: "file://" + up}
	ctx := context.Background()
	l, err := NewLocal(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !l.git {
		t.Fatal("clone 下來的目錄要當成 git repo 讀")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.md")); !os.IsNotExist(err) {
		t.Fatalf("不該 checkout 工作目錄：%v", err)
	}
	if b, err := l.Read(ctx, "a.md"); err != nil || string(b) != "v1\n" {
		t.Fatalf("Read = %q, %v", b, err)
	}

	// 遠端更新後，第二次 NewLocal 不重新 clone（目錄已存在），由 Fetch 抓新內容
	os.WriteFile(filepath.Join(up, "a.md"), []byte("v2\n"), 0o644)
	git(t, up, "commit", "-q", "-am", "v2")
	l2, err := NewLocal(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := l2.Read(ctx, "a.md"); string(b) != "v1\n" {
		t.Fatalf("Fetch 之前應該還是 v1，得到 %q", b)
	}
	if err := l2.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := l2.Read(ctx, "a.md"); string(b) != "v2\n" {
		t.Fatalf("Fetch 之後應該是 v2，得到 %q", b)
	}
}

// clone 失敗時不留下半套目錄，也不建出目標資料夾，下一次會重試。
func TestCloneFailureLeavesNothing(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "work")
	_, err := NewLocal(context.Background(), config.Source{Name: "w", Dir: dir, Ref: "main", Clone: "file://" + filepath.Join(parent, "no-such-repo")})
	if err == nil {
		t.Fatal("want error")
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 0 {
		t.Fatalf("留下了 %v", entries)
	}
}

// 沒設 clone 時，資料夾不存在照舊是錯誤（本機設定打錯路徑時不會默默去抓）。
func TestMissingDirWithoutClone(t *testing.T) {
	_, err := NewLocal(context.Background(), config.Source{Name: "w", Dir: filepath.Join(t.TempDir(), "nope"), Ref: "main"})
	if err == nil {
		t.Fatal("want error")
	}
}

// clone 不展開工作目錄，.gitattributes 只存在版本裡。Read 要照「那個版本」的 .gitattributes 套 filter，
// 不然 LFS 的檔案讀到的是指標檔（2026-11-05 實際踩過：xlsx 讀成 "version https://git-lfs…"）。
// 這裡用一個把字轉大寫的 filter 代替 LFS，測試環境不必裝 git-lfs。
func TestReadAppliesFilterFromRefAttributes(t *testing.T) {
	up := t.TempDir()
	git(t, up, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(up, ".gitattributes"), []byte("*.up filter=upper\n"), 0o644)
	os.WriteFile(filepath.Join(up, "a.up"), []byte("hello\n"), 0o644)
	git(t, up, "add", ".")
	git(t, up, "commit", "-q", "-m", "v1")

	dir := filepath.Join(t.TempDir(), "work")
	ctx := context.Background()
	l, err := NewLocal(ctx, config.Source{Name: "w", Dir: dir, Ref: "main", Fetch: true, Clone: "file://" + up})
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "filter.upper.smudge", "tr a-z A-Z")
	if b, err := l.Read(ctx, "a.up"); err != nil || string(b) != "HELLO\n" {
		t.Fatalf("Read = %q, %v; want filter applied", b, err)
	}
}
