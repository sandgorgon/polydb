package polydb_test

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/sandgorgon/polydb"
)

// docBlock is a fenced Rhizome example in the documentation.
type docBlock struct {
	line    int
	text    string
	wantErr bool // ```rhizome error: the example shows a mistake
}

// docBlocks reads the ```rhizome blocks of a Markdown file. Blocks
// marked norun (fragments, or ones that need a server) are skipped.
func docBlocks(t *testing.T, path string) []docBlock {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []docBlock
	var cur *docBlock
	var b strings.Builder
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if cur != nil {
			if trimmed == "```" {
				cur.text = b.String()
				out = append(out, *cur)
				cur = nil
				continue
			}
			b.WriteString(line + "\n")
			continue
		}
		if !strings.HasPrefix(trimmed, "```rhizome") {
			continue
		}
		words := strings.Fields(strings.TrimPrefix(trimmed, "```"))
		skip := false
		blk := docBlock{line: n}
		for _, w := range words[1:] {
			switch w {
			case "norun":
				skip = true
			case "error":
				blk.wantErr = true
			default:
				t.Fatalf("%s:%d: unknown block option %q", path, n, w)
			}
		}
		if skip {
			// Consume the block without running it.
			for sc.Scan() {
				n++
				if strings.TrimSpace(sc.Text()) == "```" {
					break
				}
			}
			continue
		}
		b.Reset()
		cur = &blk
	}
	if cur != nil {
		t.Fatalf("%s:%d: unclosed code block", path, cur.line)
	}
	return out
}

// TestDocs runs every Rhizome example in README.md and docs/, in order,
// against a fresh database per page, so the documentation can't drift
// from what PolyDB does.
func TestDocs(t *testing.T) {
	pages, _ := filepath.Glob("docs/*.md")
	pages = append([]string{"README.md"}, pages...)
	for _, page := range pages {
		if _, err := os.Stat(page); err != nil {
			continue
		}
		t.Run(page, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			db, err := polydb.Open(dir, &polydb.Options{BackupDir: filepath.Join(dir, "backups")})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, blk := range docBlocks(t, page) {
				_, err := db.Exec(ctx, blk.text, nil)
				switch {
				case blk.wantErr && err == nil:
					t.Errorf("%s:%d: the example should fail, but ran:\n%s", page, blk.line, blk.text)
				case !blk.wantErr && err != nil:
					t.Errorf("%s:%d: %v\n%s", page, blk.line, err, blk.text)
				}
			}
		})
	}
}

// TestDocLinks checks that links between the documentation's pages lead
// somewhere: the file exists and, for #anchor, has that heading.
func TestDocLinks(t *testing.T) {
	pages, _ := filepath.Glob("docs/*.md")
	pages = append([]string{"README.md"}, pages...)
	link := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	for _, page := range pages {
		data, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range link.FindAllStringSubmatch(stripCode(string(data)), -1) {
			target := m[1]
			if strings.Contains(target, "://") {
				continue
			}
			file, anchor, _ := strings.Cut(target, "#")
			path := page
			if file != "" {
				path = filepath.Join(filepath.Dir(page), file)
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("%s: link to %s: no such file", page, target)
				continue
			}
			if anchor == "" || !strings.HasSuffix(path, ".md") {
				continue
			}
			if !hasAnchor(t, path, anchor) {
				t.Errorf("%s: link to %s: no heading for #%s", page, target, anchor)
			}
		}
	}
}

// stripCode removes fenced code blocks, whose text isn't links.
func stripCode(s string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			in = !in
			continue
		}
		if !in {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// hasAnchor reports whether a Markdown file has a heading whose GitHub
// anchor is anchor.
func hasAnchor(t *testing.T, path, anchor string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(stripCode(string(data)), "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		if slug(strings.TrimLeft(line, "# ")) == anchor {
			return true
		}
	}
	return false
}

// slug is GitHub's anchor for a heading: lowercase, spaces to dashes,
// punctuation other than - and _ dropped.
func slug(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}
