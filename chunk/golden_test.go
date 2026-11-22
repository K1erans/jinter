package chunk

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/golden.txt from Split's current output")

// goldenOptions cut the golden files several ways: as searches do, one
// piece per definition, and with limits small enough that definitions are
// opened up, nested ones too.
var goldenOptions = []Options{
	Default,
	{MinLines: 1, MaxLines: 300, MaxBytes: 1 << 20, ParseTimeout: 30 * time.Second},
	{MinLines: 1, MaxLines: 12, MaxBytes: 400, ParseTimeout: 30 * time.Second},
	{MinLines: 3, MaxLines: 6, MaxBytes: 1 << 20, ParseTimeout: 30 * time.Second},
}

// TestSplitMatchesGolden pins Split's output, byte for byte, on a file per
// language: a change that moves any chunk fails it. Only regenerate the
// golden file (go test ./chunk -run Golden -update) together with a bump
// of Version.
func TestSplitMatchesGolden(t *testing.T) {
	files, err := filepath.Glob("testdata/golden/*")
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden files: %v", err)
	}
	slices.Sort(files)
	var b strings.Builder
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		for i, opts := range goldenOptions {
			for _, c := range Split(name, src, opts) {
				fmt.Fprintf(&b, "%s #%d: %d-%d @%d+%d %q\n", name, i, c.Start, c.End, c.Offset, len(c.Code), c.Signature)
			}
		}
	}
	got := b.String()
	if *update {
		if err := os.WriteFile("testdata/golden.txt", []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile("testdata/golden.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := range max(len(gl), len(wl)) {
			g, w := "", ""
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("Split's output changed; first difference at line %d:\n got  %s\n want %s", i+1, g, w)
			}
		}
	}
}
