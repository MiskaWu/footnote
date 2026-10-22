// Package source 列出並讀取知識來源的檔案。v1 測試期只有本機 checkout（Local）。
package source

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/miskawu/footnote/internal/config"
)

// Local 讀本機目錄。目錄是 git repo 根目錄時，讀 cfg.Ref 已 commit 的內容（LFS 會經由
// filter 還原），blob sha 也取自 git——跟 GitLab tree API 給的一致；否則直接讀檔案。
type Local struct {
	Cfg config.Source
	git bool
}

func NewLocal(ctx context.Context, cfg config.Source) (*Local, error) {
	if _, err := os.Stat(cfg.Dir); err != nil {
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

func (l *Local) Read(ctx context.Context, path string) ([]byte, error) {
	if !l.git {
		return os.ReadFile(filepath.Join(l.Cfg.Dir, filepath.FromSlash(path)))
	}
	cmd := exec.CommandContext(ctx, "git", "-C", l.Cfg.Dir, "cat-file", "--filters", l.Cfg.Ref+":"+path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file %s: %v: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (l *Local) listGit(ctx context.Context) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", l.Cfg.Dir, "ls-tree", "-r", "-z", l.Cfg.Ref)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %v: %s", l.Cfg.Ref, err, strings.TrimSpace(stderr.String()))
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
