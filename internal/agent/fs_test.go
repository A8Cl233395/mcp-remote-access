package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTruncatesSmallFileExceedingMaxLines(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 2500; i++ {
		sb.WriteString("line\n")
	}
	// ~12.5 KB (< MaxReadBytes=50KB), but 2500 lines (> MaxReadLines=2000)
	filePath := filepath.Join(dir, "many_lines.txt")
	if err := os.WriteFile(filePath, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}

	out, meta, err := ReadFileOrDir("many_lines.txt", dir, nil, nil)
	if err != nil {
		t.Fatalf("ReadFileOrDir failed: %v", err)
	}
	if meta["truncated"] != true {
		t.Fatalf("expected truncated=true for 2500-line file (<50KB), got meta=%+v", meta)
	}
	if !strings.Contains(out, "lines 1-2000") || !strings.Contains(out, "offset: 2001") {
		t.Fatalf("expected output truncated at line 2000 with offset 2001, got:\n%s", out[:min(len(out), 300)])
	}
	if strings.Contains(out, "\n2001: ") {
		t.Fatalf("output should not contain line 2001 when capped at MaxReadLines")
	}
}

func TestCompileGlobRegexBraceWildcards(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "src"), 0755)
	_ = os.MkdirAll(filepath.Join(dir, "test"), 0755)
	_ = os.MkdirAll(filepath.Join(dir, "docs"), 0755)
	_ = os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "test", "a_test.go"), []byte("package test"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "docs", "note.go"), []byte("package docs"), 0644)

	out, meta, err := GlobFiles(dir, "{src/*.go,test/*.go}", "", false, 100)
	if err != nil {
		t.Fatalf("GlobFiles failed: %v", err)
	}
	if meta["count"] != 2 || !strings.Contains(out, "main.go") || !strings.Contains(out, "a_test.go") {
		t.Fatalf("expected {src/*.go,test/*.go} to match 2 files, got meta=%+v out=%q", meta, out)
	}
	if strings.Contains(out, "note.go") {
		t.Fatalf("did not expect docs/note.go to match, got:\n%s", out)
	}
}
