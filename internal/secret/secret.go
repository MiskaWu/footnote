// Package secret 在入庫前掃描密鑰。命中的檔案整檔不收。
package secret

import (
	"regexp"
	"strings"
)

type Finding struct {
	Rule string
	Line int
}

type rule struct {
	name string
	re   *regexp.Regexp
	// valueGroup > 0 時，該群組若像佔位符就不算命中。
	valueGroup int
}

var rules = []rule{
	{name: "gitlab_pat", re: regexp.MustCompile(`glpat-[0-9A-Za-z_\-]{20,}`)},
	{name: "slack_token", re: regexp.MustCompile(`xox[abpr]-[0-9A-Za-z\-]{10,}`)},
	{name: "aws_access_key", re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{name: "private_key", re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{name: "assignment", re: regexp.MustCompile(`(?i)(password|passwd|secret|token)["']?\s*[:=]\s*(\S{8,})`), valueGroup: 2},
}

var placeholder = regexp.MustCompile(`(?i)^["']?(<.*>|\$\{.*\}|\{\{.*\}\}|\$[A-Z_]+|.*(xxx|changeme|example|your[_-]|\*\*\*|placeholder|dummy).*)["']?,?$`)

// Scan 回傳 text 裡所有命中的規則與行號（從 1 起算）。
func Scan(text string) []Finding {
	var out []Finding
	for i, line := range strings.Split(text, "\n") {
		for _, r := range rules {
			for _, m := range r.re.FindAllStringSubmatch(line, -1) {
				if r.valueGroup > 0 && placeholder.MatchString(m[r.valueGroup]) {
					continue
				}
				out = append(out, Finding{Rule: r.name, Line: i + 1})
			}
		}
	}
	return out
}
