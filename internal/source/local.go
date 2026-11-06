// Package source 列出並讀取知識來源的檔案。v1 測試期只有本機 checkout（Local）。
package source

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/miskawu/footnote/internal/config"
)

// Local 讀本機目錄。目錄是 git repo 根目錄時，讀 cfg.Ref 已 commit 的內容（LFS 會經由
// filter 還原），blob sha 也取自 git——跟 GitLab tree API 給的一致；否則直接讀檔案。
type Local struct {
	Cfg config.Source
	git bool
}

// Reader 是入庫看到的知識來源：本機 checkout（Local）或資料庫（DB）。
type Reader interface {
	Config() config.Source
	List(ctx context.Context) (map[string]string, error) // 所有檔案的 path → sha
	Selected(all map[string]string) map[string]string    // 依設定篩出要索引的檔案
	IsDraft(path string) bool
	Read(ctx context.Context, path string) ([]byte, error)
}

func NewLocal(ctx context.Context, cfg config.Source) (*Local, error) {
	_, err := os.Stat(cfg.Dir)
	if errors.Is(err, fs.ErrNotExist) && cfg.Clone != "" {
		err = clone(ctx, cfg)
	}
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", cfg.Name, err)
	}
	l := &Local{Cfg: cfg}
	out, err := exec.CommandContext(ctx, "git", "-C", cfg.Dir, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		top, _ := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
		dir, _ := filepath.EvalSymlinks(cfg.Dir)
		l.git = top == dir
	}
	return l, nil
}

func (l *Local) Config() config.Source { return l.Cfg }

// List 回傳來源裡所有檔案（不經 include 篩選）的 path → blob sha。
func (l *Local) List(ctx context.Context) (map[string]string, error) {
	if l.git {
		return l.listGit(ctx)
	}
	return l.listDir()
}

// Selected 依 include/exclude 篩出要索引的檔案。
func (l *Local) Selected(all map[string]string) map[string]string {
	out := map[string]string{}
	for p, sha := range all {
		if matchAny(l.Cfg.Include, p) && !matchAny(l.Cfg.Exclude, p) {
			out[p] = sha
		}
	}
	return out
}

func (l *Local) IsDraft(path string) bool { return matchAny(l.Cfg.Draft, path) }

// readRef 是實際讀取的 ref：設了 fetch 就讀 origin/<ref>（遠端最新），否則讀本機的 <ref>。
func (l *Local) readRef() string {
	if l.Cfg.Fetch {
		return "origin/" + l.Cfg.Ref
	}
	return l.Cfg.Ref
}

// Fetch 從 origin 抓最新的 commit。只更新遠端參照，不動本機 checkout 的工作目錄與分支。
// 不互動：要密碼或確認主機時直接失敗，由呼叫端記 warn、沿用上次抓到的內容。
func (l *Local) Fetch(ctx context.Context) error {
	if !l.git || !l.Cfg.Fetch {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", l.Cfg.Dir, "fetch", "--quiet", "origin")
	cmd.Env = gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git fetch %s: %v: %s", l.Cfg.Dir, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// clone 把 cfg.Clone 抓到 cfg.Dir。先抓到旁邊的暫存目錄、成功才改名，中途失敗不會留下半套 repo
// 讓下一次誤以為已經 clone 好。--filter=blob:none 加 --no-checkout：只抓 commit 與目錄樹，
// 讀檔時（cat-file）才向遠端要那一個檔，LFS 也是讀到才下載；設計文件的完整歷史有 2GB 以上，用不到。
func clone(ctx context.Context, cfg config.Source) error {
	parent := filepath.Dir(cfg.Dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(cfg.Dir)+".clone-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // 改名成功後 tmp 已不存在，這行什麼都不做
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "clone", "--quiet", "--no-checkout", "--filter=blob:none",
		"--branch", cfg.Ref, cfg.Clone, tmp)
	cmd.Env = gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s: %v: %s", cfg.Clone, err, strings.TrimSpace(stderr.String()))
	}
	return os.Rename(tmp, cfg.Dir)
}

// gitEnv：碰遠端的 git 指令一律不互動，要密碼或確認主機時直接失敗，不會卡住。
func gitEnv() []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return env
}

func (l *Local) Read(ctx context.Context, path string) ([]byte, error) {
	if !l.git {
		return os.ReadFile(filepath.Join(l.Cfg.Dir, filepath.FromSlash(path)))
	}
	// attr.tree（git 2.46+）：LFS 等 filter 的設定（.gitattributes）從要讀的那個版本取，不看工作目錄。
	// clone 時沒有展開工作目錄，不加這個會讀到 LFS 的指標檔；本機 checkout 的工作目錄也可能跟 ref 不同版。
	// 不用 --attr-source：它會經由環境變數傳給補抓內容時遠端的 upload-pack，遠端解析不到這個 ref 就整個失敗；
	// attr.tree 解析不到時當成空的，不會出錯。
	// 讀檔可能要連遠端（部分 clone 補抓內容、LFS 下載），所以跟 fetch 一樣不互動。
	cmd := exec.CommandContext(ctx, "git", "-C", l.Cfg.Dir, "-c", "attr.tree="+l.readRef(),
		"cat-file", "--filters", l.readRef()+":"+path)
	cmd.Env = gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file %s: %v: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (l *Local) listGit(ctx context.Context) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", l.Cfg.Dir, "ls-tree", "-r", "-z", l.readRef())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %v: %s", l.readRef(), err, strings.TrimSpace(stderr.String()))
	}
	files := map[string]string{}
	for _, rec := range bytes.Split(out, []byte{0}) {
		// <mode> SP <type> SP <sha> TAB <path>
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			continue
		}
		f := strings.Fields(string(meta))
		if len(f) == 3 && f[1] == "blob" {
			files[string(path)] = f[2]
		}
	}
	return files, nil
}

func (l *Local) listDir() (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(l.Cfg.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(l.Cfg.Dir, p)
		files[filepath.ToSlash(rel)] = BlobSHA(b)
		return nil
	})
	return files, err
}

// BlobSHA 算 git 的 blob sha，讓非 git 目錄的 sha 與 git 的定義一致。
func BlobSHA(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func matchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if ok, _ := doublestar.Match(p, path); ok {
			return true
		}
	}
	return false
}
