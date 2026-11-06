package qa

import (
	"fmt"
	"path"
	"strings"

	"github.com/miskawu/footnote/internal/store"
)

// Label 是片段的顯示名稱：專案末段 · 檔名 › 標題（xlsx 附列號）。回覆的連結文字與給模型看的出處都用它。
func Label(c store.Candidate) string {
	project := c.ProjectPath[strings.LastIndex(c.ProjectPath, "/")+1:]
	s := fmt.Sprintf("%s · %s › %s", project, path.Base(c.Path), c.Heading)
	if isSheet(c.Path) && c.LineStart > 0 {
		if c.LineEnd > c.LineStart {
			s += fmt.Sprintf("（第 %d–%d 列）", c.LineStart, c.LineEnd)
		} else {
			s += fmt.Sprintf("（第 %d 列）", c.LineStart)
		}
	}
	return s
}

// Note 是片段的版本標註：舊版優先於未確認設計（入庫時已定好，見 ingest）；沒有標註回傳空字串。
// 回覆裡的標籤與給模型看的 note 都用它。
func Note(c store.Candidate) string {
	switch {
	case c.SupersededBy != "":
		return "舊版，新版見 " + strings.TrimSuffix(path.Base(c.SupersededBy), path.Ext(c.SupersededBy))
	case c.Draft:
		return "未確認設計"
	}
	return ""
}

func isSheet(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	return ext == ".xlsx" || ext == ".xlsm"
}
