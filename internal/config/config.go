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
	DatabaseURLEnv string    `yaml:"database_url_env"`
	GitLabURL      string    `yaml:"gitlab_url"` // 組連結用
	Sources        []Source  `yaml:"sources"`
	FAQ            *Source   `yaml:"faq"`
	Anthropic      Anthropic `yaml:"anthropic"`
	TEI            TEI       `yaml:"tei"`
	QA             QA        `yaml:"qa"`
	Bot            Bot       `yaml:"bot"`
	Server         Server    `yaml:"server"`
	Sync           Sync      `yaml:"sync"`

	path string // 設定檔所在目錄，用來解析相對路徑
}

// 來源的種類。文件來源是 local；FAQ 來源固定是 db，內容存在資料庫（kb_changes），由提案與審核修改。
const (
	TypeLocal = "local"
	TypeDB    = "db"
)

// Source 是一個知識來源。文件讀 git checkout（local）；FAQ 卡與詞彙表存在資料庫（db）。
type Source struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`         // local；FAQ 來源由程式設成 db
	ProjectPath string `yaml:"project_path"` // GitLab 上的專案路徑，顯示與組連結用
	Dir         string `yaml:"dir"`
	Ref         string `yaml:"ref"`   // git ref；dir 是 git repo 時讀這個 ref 的內容（也是連結用的分支名）
	Fetch       bool   `yaml:"fetch"` // 同步前先 git fetch origin，改讀 origin/<ref>；本機 checkout 的工作目錄不動
	// Clone：dir 不存在時從這個網址 clone（容器裡用；本機已有 checkout 就不設）。只抓 commit 與目錄樹，
	// 檔案內容（含 LFS）讀到時才下載。https 的帳密由 git 的 credential helper 給（見 Dockerfile）。
	Clone      string   `yaml:"clone"`
	Include    []string `yaml:"include"`
	Exclude    []string `yaml:"exclude"`
	Draft      []string `yaml:"draft"`       // 命中的檔案在回覆標「未確認設計」
	SkipSheets []string `yaml:"skip_sheets"` // xlsx 分頁名稱含這些字就略過（不分大小寫）
	MaxTextKB  int      `yaml:"max_text_kb"` // 抽出的文字超過就略過整檔
	// Versions：同一系統的多份文件，每組由新到舊列出路徑。組裡第一份以外的都是舊版：
	// 回覆時排在同組新版的後面，標「舊版，新版見 …」（不再標「未確認設計」）。
	Versions [][]string `yaml:"versions"`
}

// SupersededBy 回傳 p 所在版本群組的最新一份；p 不在任何群組、或本身就是最新的，回傳空字串。
func (s *Source) SupersededBy(p string) string {
	for _, g := range s.Versions {
		for i, v := range g {
			if v == p && i > 0 {
				return g[0]
			}
		}
	}
	return ""
}

func (s *Source) checkVersions() error {
	seen := map[string]bool{}
	for _, g := range s.Versions {
		if len(g) < 2 {
			return fmt.Errorf("source %q: versions 的每一組至少要兩份文件：%v", s.Name, g)
		}
		for _, p := range g {
			if seen[p] {
				return fmt.Errorf("source %q: %s 在 versions 裡出現不只一次", s.Name, p)
			}
			seen[p] = true
		}
	}
	return nil
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
	TauFollow    float64 `yaml:"tau_follow"` // 討論串追問時，本題單獨的重排分數也要過這個門檻
	TauKeep      float64 `yaml:"tau_keep"`
	TauDup       float64 `yaml:"tau_dup"`
	KMax         int     `yaml:"k_max"` // 送進生成的段落上限
	HistoryTurns int     `yaml:"history_turns"`
}

// Anthropic 是模型整理回答的設定。API key 只走環境變數；沒設就不整理，照舊回原文摘錄。
type Anthropic struct {
	APIKeyEnv        string  `yaml:"api_key_env"`
	AnswerModel      string  `yaml:"answer_model"`
	RewriteModel     string  `yaml:"rewrite_model"` // 閘門 A 沒過時翻譯補查用的模型；空字串表示不補查
	Effort           string  `yaml:"effort"`
	MaxTokens        int64   `yaml:"max_tokens"`         // thinking 也計入
	MonthlyBudgetUSD float64 `yaml:"monthly_budget_usd"` // 本月累計超過就退回原文摘錄
	Style            string  `yaml:"style"`              // 回答的寫法（語氣、長度、格式、人設）；只根據文件、標出處的規則寫在程式裡，不能從這裡改
}

type Bot struct {
	TokenEnv string `yaml:"token_env"`
	// Channels 是「哪裡可以問、能搜哪些專案」的唯一一張表。
	// key 是 Discord 頻道 id（討論串算它的上層頻道），或保留字 ScopeDM、ScopeWeb、ScopeAny；
	// 值是允許的專案前綴，空清單代表全部專案。表上沒有的地方一律不回答。
	Channels map[string][]string `yaml:"channels"`
	// ReviewChannel 是 FAQ 提案貼出來等人審的 Discord 頻道；Reviewers 是能按核准、退回的 Discord 使用者 id。
	// 兩個都沒設時提案會一直待審：核准只能由人在 Discord 按，指令列沒有這條路（代理不能審自己的卡）。
	ReviewChannel string   `yaml:"review_channel"`
	Reviewers     []string `yaml:"reviewers"`
	RateLimit     struct {
		PerMinute int `yaml:"per_minute"`
		PerDay    int `yaml:"per_day"`
	} `yaml:"rate_limit"`
}

// bot.channels 的保留 key。
const (
	ScopeDM  = "dm"  // 所有私訊
	ScopeWeb = "web" // 本機測試頁
	ScopeAny = "*"   // 表上沒列到的其他地方，含私訊與測試頁；只給本機開發用
)

// Scope 查某個地方能不能問、能搜哪些專案（prefixes 空代表全部）。沒列到、也沒有 ScopeAny 時 ok 是 false。
func (b Bot) Scope(key string) (prefixes []string, ok bool) {
	if p, ok := b.Channels[key]; ok {
		return p, true
	}
	p, ok := b.Channels[ScopeAny]
	return p, ok
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
		c.Sources[i].fill(c.path, c.Sources[i].ProjectPath)
	}
	if c.FAQ != nil {
		if c.FAQ.Dir != "" {
			return nil, fmt.Errorf("faq.dir 已經不用了：FAQ 卡改存資料庫，請拿掉 dir，第一次用 footnote faq import <目錄> 匯入")
		}
		c.FAQ.fill(c.path, "faq")
		c.FAQ.Type = TypeDB
	}
	return c, c.validate()
}

func Default() *Config {
	return &Config{
		DatabaseURLEnv: "FOOTNOTE_DATABASE_URL",
		Anthropic:      Anthropic{APIKeyEnv: "ANTHROPIC_API_KEY", Effort: "low", MaxTokens: 8000},
		TEI:            TEI{EmbedDim: 1024, Timeout: 60 * time.Second},
		QA: QA{
			Candidates: 30, KReply: 3, ExcerptChars: 200,
			TauAnswer: 0.35, TauFollow: 0.15, TauKeep: 0.15, TauDup: 0.85, HistoryTurns: 1,
		},
		Server: Server{Addr: "127.0.0.1:28090"},
		Sync:   Sync{Interval: 10 * time.Minute},
	}
}

// fill 補上預設值。沒寫 name 時用 defaultName：文件來源傳 project_path，FAQ 來源傳 "faq"
// （由呼叫端決定，不靠「先 fill 再補」的順序，之前就是因為順序錯了 FAQ 一直拿到 project_path）。
func (s *Source) fill(base, defaultName string) {
	if strings.HasPrefix(s.Dir, "~/") {
		home, _ := os.UserHomeDir()
		s.Dir = filepath.Join(home, s.Dir[2:])
	} else if s.Dir != "" && !filepath.IsAbs(s.Dir) {
		s.Dir = filepath.Join(base, s.Dir)
	}
	if s.Type == "" {
		s.Type = TypeLocal
	}
	if s.Ref == "" {
		s.Ref = "main"
	}
	if s.MaxTextKB == 0 {
		s.MaxTextKB = 512
	}
	if s.Name == "" {
		s.Name = defaultName
	}
}

func (c *Config) validate() error {
	seen := map[string]bool{}
	all := append([]Source{}, c.Sources...)
	if c.FAQ != nil {
		all = append(all, *c.FAQ)
	}
	for _, s := range all {
		if s.ProjectPath == "" {
			return fmt.Errorf("source %q: project_path 必填", s.Name)
		}
		if seen[s.ProjectPath] {
			return fmt.Errorf("source %q: project_path 重複", s.Name)
		}
		seen[s.ProjectPath] = true
		if s.Type == TypeDB {
			continue // FAQ 來源：內容在資料庫，沒有版本群組
		}
		if s.Type != TypeLocal {
			return fmt.Errorf("source %q: 文件來源只支援 type=local", s.Name)
		}
		if s.Dir == "" {
			return fmt.Errorf("source %q: dir 必填", s.Name)
		}
		if err := s.checkVersions(); err != nil {
			return err
		}
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
