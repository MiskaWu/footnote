package secret

import "testing"

func TestScan(t *testing.T) {
	hits := []struct{ rule, text string }{
		{"gitlab_pat", "token glpat-abcdefghijklmnopqrstu1"},
		{"slack_token", "SLACK=xoxb-1234567890-abcdef"},
		{"aws_access_key", "key AKIAABCDEFGHIJKLMNOP"},
		{"private_key", "-----BEGIN RSA PRIVATE KEY-----"},
		{"private_key", "-----BEGIN PRIVATE KEY-----"},
		{"assignment", "password: hunter2hunter2"},
		{"assignment", "DB_TOKEN = 9f8e7d6c5b4a"},
	}
	for _, h := range hits {
		got := Scan(h.text)
		if len(got) == 0 || got[0].Rule != h.rule {
			t.Errorf("Scan(%q) = %v, want rule %s", h.text, got, h.rule)
		}
	}

	misses := []string{
		"password: <your-password>",
		"token: ${GITLAB_TOKEN}",
		"secret: {{ .Values.secret }}",
		"password: changeme123",
		"token = $GITLAB_TOKEN",
		"password: short",
		"token：使用者登入後取得的憑證字串",
		"glpat-short",
		"AKIA1234",
	}
	for _, m := range misses {
		if got := Scan(m); len(got) != 0 {
			t.Errorf("Scan(%q) = %v, want none", m, got)
		}
	}
}

func TestScanLineNumbers(t *testing.T) {
	got := Scan("ok\nok\npassword: hunter2hunter2")
	if len(got) != 1 || got[0].Line != 3 {
		t.Fatalf("got %v, want line 3", got)
	}
}
