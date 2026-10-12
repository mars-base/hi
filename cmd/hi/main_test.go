package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProjectSessions(t *testing.T) {
	home := t.TempDir()
	// os.UserHomeDir reads $HOME on unix and %USERPROFILE% on Windows.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// /tmp/fake_proj.x → dash-escaping of '/', '_', '.'
	dir := "/tmp/fake_proj.x"
	projDir := filepath.Join(home, ".claude", "projects", "-tmp-fake-proj-x")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, mtime time.Time) string {
		p := filepath.Join(projDir, name)
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := write("aaa.jsonl", time.Now().Add(-time.Hour))
	newest := write("bbb.jsonl", time.Now())
	write("not-jsonl.txt", time.Now())

	got := projectSessions(dir)
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions, got %d: %v", len(got), got)
	}
	if got[0] != newest || got[1] != old {
		t.Fatalf("expected newest-first order, got %v", got)
	}

	if s := projectSessions("/tmp/does-not-exist"); s != nil {
		t.Fatalf("expected nil for unknown dir, got %v", s)
	}
}
