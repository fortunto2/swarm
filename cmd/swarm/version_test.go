package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStaleDaemon(t *testing.T) {
	dir := t.TempDir()
	if s := staleDaemon(dir, "abc1234"); s != "" {
		t.Fatalf("no daemon.rev yet, still warned: %q", s)
	}
	for _, c := range []struct {
		ran, self string
		warn      bool
	}{
		{"abc1234", "abc1234", false},
		{"abc1234", "def5678", true},
		{"abc1234", "abc1234+dirty", true},
		{"", "def5678", false}, // a daemon built without a VCS stamp
		{"abc1234", "", false}, // this binary has none
	} {
		if err := os.WriteFile(filepath.Join(dir, revFile), []byte(c.ran+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := staleDaemon(dir, c.self) != ""; got != c.warn {
			t.Errorf("ran %q self %q: warned %v, want %v", c.ran, c.self, got, c.warn)
		}
	}
}
