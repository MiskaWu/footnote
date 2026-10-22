// Package config 讀取 footnote 的 YAML 設定；secret 只走環境變數。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DatabaseURLEnv string   `yaml:"database_url_env"`
	GitLabURL      string   `yaml:"gitlab_url"` // 組連結用
	Sources        []Source `yaml:"sources"`
	FAQ            *Source  `yaml:"faq"`
	TEI            TEI      `yaml:"tei"`
	QA             QA       `yaml:"qa"`
	Bot            Bot      `yaml:"bot"`
	Server         Server   `yaml:"server"`
	Sync           Sync     `yaml:"sync"`

	path string // 設定檔所在目錄，用來解析相對路徑
}

// Source 是一個知識來源。v1 測試期只有 local（讀本機 checkout）。
type Source struct {
	Name        string   `yaml:"name"`
	Type        string   `yaml:"type"`         // local
	ProjectPath string   `yaml:"project_path"` // GitLab 上的專案路徑，顯示與組連結用
	Dir         string   `yaml:"dir"`
	Ref         string   `yaml:"ref"` // git ref；dir 是 git repo 時讀這個 ref 的內容
	Include     []string `yaml:"include"`
	Exclude     []string `yaml:"exclude"`
	Draft       []string `yaml:"draft"`       // 命中的檔案在回覆標「草稿」
	SkipSheets  []string `yaml:"skip_sheets"` // xlsx 分頁名稱含這些字就略過（不分大小寫）
	MaxTextKB   int      `yaml:"max_text_kb"` // 抽出的文字超過就略過整檔
}

type TEI struct {
	EmbedURL    string        `yaml:"embed_url"`
	RerankURL   string        `yaml:"rerank_url"`
	EmbedDim    int           `yaml:"embed_dim"`
	QueryPrefix string        `yaml:"query_prefix"`
	Timeout     time.Duration `yaml:"timeout"`
}

type QA struct {
	Candidates   int     `yaml:"candidates"`
	KReply       int     `yaml:"k_reply"`
	ExcerptChars int     `yaml:"excerpt_chars"`
	TauAnswer    float64 `yaml:"tau_answer"`
	TauKeep      float64 `yaml:"tau_keep"`
	TauDup       float64 `yaml:"tau_dup"`
	HistoryTurns int     `yaml:"history_turns"`
}

type Bot struct {
	TokenEnv  string              `yaml:"token_env"`
	Channels  map[string][]string `yaml:"channels"` // 頻道 id → 允許的專案前綴
	RateLimit struct {
		PerMinute int `yaml:"per_minute"`
		PerDay    int `yaml:"per_day"`
	} `yaml:"rate_limit"`
}

type Server struct {
	Addr string `yaml:"addr"`
}

type Sync struct {
	Interval time.Duration `yaml:"interval"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := Default()
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	abs, _ := filepath.Abs(path)
	c.path = filepath.Dir(abs)
	for i := range c.Sources {
		c.Sources[i].fill(c.path)
	}
	if c.FAQ != nil {
		c.FAQ.fill(c.path)
		if c.FAQ.Name == "" {
			c.FAQ.Name = "faq"
		}
	}
	return c, c.validate()
}

func Default() *Config {
	return &Config{
		DatabaseURLEnv: "FOOTNOTE_DATABASE_URL",
		TEI:            TEI{EmbedDim: 1024, Timeout: 60 * time.Second},
		QA: QA{
			Candidates: 30, KReply: 3, ExcerptChars: 200,
			TauAnswer: 0.35, TauKeep: 0.15, TauDup: 0.85, HistoryTurns: 1,
		},
		Server: Server{Addr: "127.0.0.1:28090"},
		Sync:   Sync{Interval: 10 * time.Minute},
	}
}

func (s *Source) fill(base string) {
	if strings.HasPrefix(s.Dir, "~/") {
		home, _ := os.UserHomeDir()
		s.Dir = filepath.Join(home, s.Dir[2:])
	} else if s.Dir != "" && !filepath.IsAbs(s.Dir) {
		s.Dir = filepath.Join(base, s.Dir)
	}
	if s.Type == "" {
		s.Type = "local"
	}
	if s.Ref == "" {
		s.Ref = "main"
	}
	if s.MaxTextKB == 0 {
		s.MaxTextKB = 512
	}
	if s.Name == "" {
		s.Name = s.ProjectPath
	}
}

func (c *Config) validate() error {
	seen := map[string]bool{}
	all := append([]Source{}, c.Sources...)
	if c.FAQ != nil {
		all = append(all, *c.FAQ)
	}
	for _, s := range all {
		if s.Type != "local" {
			return fmt.Errorf("source %q: 目前只支援 type=local", s.Name)
		}
		if s.ProjectPath == "" || s.Dir == "" {
			return fmt.Errorf("source %q: project_path 與 dir 必填", s.Name)
		}
		if seen[s.ProjectPath] {
			return fmt.Errorf("source %q: project_path 重複", s.Name)
		}
		seen[s.ProjectPath] = true
	}
	if c.TEI.EmbedURL == "" || c.TEI.RerankURL == "" {
		return fmt.Errorf("tei.embed_url 與 tei.rerank_url 必填")
	}
	return nil
}

func (c *Config) DatabaseURL() (string, error) {
	v := os.Getenv(c.DatabaseURLEnv)
	if v == "" {
		return "", fmt.Errorf("環境變數 %s 未設定", c.DatabaseURLEnv)
	}
	return v, nil
}

// AllSources 回傳文件來源加上 FAQ 來源（FAQ 放最後，同步時要排在文件之後）。
func (c *Config) AllSources() []Source {
	all := append([]Source{}, c.Sources...)
	if c.FAQ != nil {
		all = append(all, *c.FAQ)
	}
	return all
}

func (c *Config) IsFAQ(projectPath string) bool {
	return c.FAQ != nil && c.FAQ.ProjectPath == projectPath
}
