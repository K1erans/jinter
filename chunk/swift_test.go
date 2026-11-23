package chunk

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/odvcencio/gotreesitter/grammars"
)

// swiftSample is a valid Swift file whose first type is big enough to be
// opened up, with braces in comments and strings the scanner must skip.
func swiftSample() string {
	var b strings.Builder
	b.WriteString(`import SwiftUI

/// A page.
@MainActor
struct Page: View {
    let title: String
    // a { brace in a comment
    var label: String { "a } brace \(title.count) in a string" }

    /* block /* nested { */ */
    func render() -> String {
        let raw = #"raw \(not) { interpolation"#
        let multi = """
            multi { line \(title)
            """
        return raw + multi
    }
`)
	for i := range 30 {
		fmt.Fprintf(&b, "\n    func step%d(_ x: Int) -> Int {\n        let y = x + %d\n        return y * 2\n    }\n", i, i)
	}
	b.WriteString(`}

#if os(macOS)
extension Page {
    func mac() -> Int { 1 }
}
#endif

enum Small { case a, b }
`)
	return b.String()
}

// unitRows lists each unit's first and last line, 1-based, in file order.
func unitRows(sl *swiftLines, units []swiftUnit) [][2]int {
	units = slices.Clone(units)
	slices.SortFunc(units, func(a, b swiftUnit) int { return a.lo - b.lo })
	var out [][2]int
	for _, u := range units {
		out = append(out, [2]int{sl.row(u.lo) + 1, sl.row(u.hi-1) + 1})
	}
	return out
}

// assertUnitsCover checks the units take src in order, each line once,
// apart from the headers and closing lines of the types opened up, which
// are in the pre and suf of the units next to them.
func assertUnitsCover(t *testing.T, src string, units []swiftUnit) {
	t.Helper()
	units = slices.Clone(units)
	slices.SortFunc(units, func(a, b swiftUnit) int { return a.lo - b.lo })
	at := 0
	var prevSuf []byte
	for _, u := range units {
		if u.lo < at || u.hi <= u.lo {
			t.Fatalf("unit %d-%d overlaps what came before %d", u.lo, u.hi, at)
		}
		gap := src[at:u.lo]
		ok := false
		for k := 0; k <= len(gap) && !ok; k++ {
			ok = strings.HasPrefix(string(prevSuf), gap[:k]) && strings.HasSuffix(string(u.pre), gap[k:])
		}
		if !ok {
			t.Fatalf("bytes %d-%d are in no unit: %q", at, u.lo, gap)
		}
		at, prevSuf = u.hi, u.suf
	}
	if !strings.HasPrefix(string(prevSuf), src[at:]) {
		t.Fatalf("bytes from %d are in no unit: %q", at, src[at:])
	}
}

func TestSwiftUnitsAreWholeDeclarations(t *testing.T) {
	src := swiftSample()
	units, sl := swiftUnits([]byte(src))
	if units == nil {
		t.Fatal("no units")
	}
	assertUnitsCover(t, src, units)

	rows := unitRows(sl, units)
	lines := strings.Split(src, "\n")
	// The big struct opens up: its first member unit starts on the line
	// after its header, and its doc comment and attribute stay in the header.
	header := slices.Index(lines, "struct Page: View {") + 1
	if rows[0] != [2]int{1, 2} || rows[1][0] != header+1 {
		t.Errorf("units start %v, want the import, then members from line %d", rows[:2], header+1)
	}
	entry := grammars.DetectLanguage("Page.swift")
	for _, u := range units {
		text := string(u.text([]byte(src)))
		if u.pre != nil && !strings.HasPrefix(text, "/// A page.\n@MainActor\nstruct Page: View {\n") {
			t.Errorf("member unit isn't inside its type's header:\n%s", text)
		}
		if strings.Contains(text, "#if") != strings.Contains(text, "#endif") {
			t.Errorf("unit splits an #if block:\n%s", text)
		}
		bt, err := parseWhole(entry, entry.Language(), []byte(text), 0)
		if err != nil {
			t.Errorf("unit doesn't parse: %v\n%s", err, text)
			continue
		}
		bt.Release()
	}
	// Every function of the struct is a unit of its own.
	if n := strings.Count(src, "func step"); len(units) < n {
		t.Errorf("%d units, want at least one per function (%d)", len(units), n)
	}
}

func TestSwiftScannerGivesUpOnWhatItCantFollow(t *testing.T) {
	for _, src := range []string{
		"struct A {\n",
		"struct A {\n    let s = \"unterminated\n}\n",
		"#endif\nstruct A {}\n",
		"#if os(iOS)\nstruct A {}\n",
		"struct A {\n    /* unterminated\n}\n",
		"struct A {\n    let x = (1]\n}\n",
	} {
		if _, ok := scanSwift([]byte(src)); ok {
			t.Errorf("scanSwift followed %q", src)
		}
		if units, _ := swiftUnits([]byte(src)); units != nil {
			t.Errorf("swiftUnits cut %q", src)
		}
	}
}

func TestSwiftScannerNeverPanics(t *testing.T) {
	src := swiftSample()
	for n := range len(src) {
		if units, _ := swiftUnits([]byte(src[:n])); units != nil {
			assertUnitsCover(t, src[:n], units)
		}
	}
}

func TestValidSwiftIsParsedWhole(t *testing.T) {
	src := []byte(swiftSample())
	tree, err := Parse("Page.swift", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree.RootNode().EndByte(); int(got) != len(src) {
		t.Errorf("tree ends at %d, want %d", got, len(src))
	}
	tree.Release()
	if _, via := split("Page.swift", src, Default, true); via != routeTree {
		t.Errorf("route %d, want tree-sitter", via)
	}
}

// One small broken declaration in a big file fails the file without the
// whole-file parse, and the file splits as it would after one.
func TestABrokenSwiftDeclarationFailsTheFileEarly(t *testing.T) {
	src := []byte(swiftSample() + "\nextension Page {\n    func broken() -> {}\n}\n")
	_, err := Parse("Page.swift", src, 0)
	if !errors.Is(err, ErrSyntax) || !strings.Contains(err.Error(), "on their own") {
		t.Fatalf("Parse error %v, want a unit's syntax error", err)
	}
	got, via := split("Page.swift", src, Default, true)
	if via != routePlain {
		t.Fatalf("route %d, want the plain splitter", via)
	}
	if plain := Split("Page.unknownext", src, Default); fmt.Sprint(got) != fmt.Sprint(plain) {
		t.Errorf("Split didn't use the plain splitter:\n got  %v\n want %v", spans(got), spans(plain))
	}
}

// Functions aren't opened up: an if condition ending in a name reads as a
// trailing closure, which gotreesitter repairs only with the code around
// it, so a statement cut out of its function can fail where the file
// parses. This file's big method holds one.
func TestSwiftTrailingClosureConditionsStayWithTheirFunction(t *testing.T) {
	var b strings.Builder
	b.WriteString("enum Snapshot {\n    static func render(_ host: Host, size: Size?) -> Size {\n")
	for i := range 100 {
		fmt.Fprintf(&b, "        host.layout(%d)\n", i)
	}
	b.WriteString("        if size == nil, host.fittingSize != host.frame.size {\n            host.resize()\n        }\n\n")
	b.WriteString("        let bounds = host.bounds\n        return bounds\n    }\n\n    static func other() {}\n}\n")
	src := []byte(b.String())
	if units, _ := swiftUnits(src); len(units) != 2 {
		t.Errorf("%d units, want the two functions", len(units))
	}
	tree, err := Parse("Snapshot.swift", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	tree.Release()
}

func TestSwiftOnlyOpensTypes(t *testing.T) {
	cases := map[string]bool{
		"struct S {":                               true,
		"@MainActor\nfinal class C: NSObject {":    true,
		"@available(macOS 13, *)\npublic enum E {": true,
		"/// Doc.\nextension View {":               true,
		"private actor A {":                        true,
		"protocol P {":                             true,
		"class func make() {":                      false,
		"static func render() -> Int {":            false,
		"var body: some View {":                    false,
		"init(x: Int) {":                           false,
		"@objc(run:) func run() {":                 false,
	}
	for header, want := range cases {
		src := []byte(header + "\n}\n")
		sl, ok := scanSwift(src)
		if !ok {
			t.Fatalf("scanSwift failed on %q", src)
		}
		if got := sl.declaresType(0, len(header)); got != want {
			t.Errorf("declaresType(%q) = %v, want %v", header, got, want)
		}
	}
}
