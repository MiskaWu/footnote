package main

import (
	"flag"
	"io"
	"testing"
)

func TestIDArg(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		id, note string
		ok       bool
	}{
		{[]string{"3", "--note", "查過 A、B"}, "3", "查過 A、B", true}, // 文件寫的順序
		{[]string{"--note", "查過 A", "G3"}, "G3", "查過 A", true},
		{[]string{"3"}, "3", "", true},
		{[]string{"3", "4"}, "", "", false},
		{[]string{"--note", "x"}, "", "x", false},
		{nil, "", "", false},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		note := fs.String("note", "", "")
		id, ok := idArg(fs, tc.args)
		if id != tc.id || ok != tc.ok || (ok && *note != tc.note) {
			t.Errorf("idArg(%q) = %q, %v, note=%q; want %q, %v, note=%q", tc.args, id, ok, *note, tc.id, tc.ok, tc.note)
		}
	}
}
