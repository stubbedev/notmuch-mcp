package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update rewrites the golden files instead of comparing against them.
var update = flag.Bool("update", false, "rewrite testdata/*.golden")

func TestHTMLToMarkdownBasics(t *testing.T) {
	got := htmlToMarkdown(`<h1>Title</h1><p>Body <b>text</b>.</p>`)
	if !strings.Contains(got, "# Title") || !strings.Contains(got, "**text**") {
		t.Errorf("got %q", got)
	}
}

// Fixtures are synthetic — real mail can't be committed — but each targets a
// pass in the pipeline: layout vs data tables, flex rows, stat headings, IE
// conditionals, hidden responsive duplicates, empty/decorative/garbage anchors,
// punctuation emphasis, invisible padding, heading compression, link rows, long lines.
func fixtures(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("testdata/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	return files
}

// TestGolden pins the rendered output. Regenerate with `just golden` and review
// the diff when a change is intentional.
func TestGolden(t *testing.T) {
	for _, f := range fixtures(t) {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			got := htmlToMarkdown(string(raw))
			golden := strings.TrimSuffix(f, ".html") + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("output drifted:\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}
