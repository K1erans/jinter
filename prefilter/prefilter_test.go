package prefilter

import (
	"reflect"
	"testing"
)

func TestKeywordsDropStopwordsAndStem(t *testing.T) {
	got := Keywords([]string{"Where do we retry failed payments?", "sends the email"})
	want := []string{"retr", "fail", "payme", "send", "emai"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestKeepsChunksContainingAnyKeyword(t *testing.T) {
	f, err := New("", []string{"retries failed payments"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"func retryCharge() {}":        true, // stem matches inside an identifier
		"// PAYMENT declined":          true, // case-insensitive
		"if err := fail(); err != nil": true,
		"func get(url string) {}":      false,
	}
	for code, want := range cases {
		if got := f.Keep(code); got != want {
			t.Errorf("%q: keep=%v, want %v", code, got, want)
		}
	}
}

func TestExplicitPatternIsUsedAsARegex(t *testing.T) {
	f, err := New(`back.?off|jitter`, []string{"retries failed payments"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Keep("exponential BACK-OFF") || !f.Keep("add jitter") || f.Keep("retry payment") {
		t.Fatal("the explicit pattern replaces the question's words")
	}
}

func TestErrors(t *testing.T) {
	if _, err := New("", []string{"where is the code that does it"}); err == nil {
		t.Error("a question of only stopwords leaves nothing to match")
	}
	if _, err := New("(", nil); err == nil {
		t.Error("invalid regex")
	}
}

func TestKeywordsMatchCaseInsensitivelyLikeTheRegex(t *testing.T) {
	f, err := New("", []string{"skip σίσυφος kelvin"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"SKIP":              true,
		"\u017fkip":         true,  // long s folds to s
		"\u212aelvin":       true,  // Kelvin sign folds to k
		"ΣΊΣΥΦΟΣ":           true,  // σίσυ, and Σ folds to σ
		"ςίσυ":              true,  // final sigma folds to σ
		"sk\xffip kel\xffv": false, // invalid bytes match nothing
		"ski":               false,
	}
	for code, want := range cases {
		if got := f.Keep(code); got != want {
			t.Errorf("%q: keep=%v, want %v", code, got, want)
		}
		if got := f.re.MatchString(code); got != want {
			t.Errorf("%q: the regex says %v, want %v", code, got, want)
		}
	}
}

// FuzzKeepMatchesTheRegex checks that the keyword search agrees with the
// regex it stands in for.
func FuzzKeepMatchesTheRegex(f *testing.F) {
	f.Add("retries failed payments", "func retryCharge() {}")
	f.Add("skip kelvin", "\u017fkip \u212aELVIN")
	f.Add("σίσυφος", "ΣΊΣΥΦΟΣ ς")
	f.Add("ǅemal ﬀoo", "ǄEMAL ǆemal FFOO")
	f.Add("abc", "a\xffbc \xe2\x84")
	f.Fuzz(func(t *testing.T, question, code string) {
		filter, err := New("", []string{question})
		if err != nil {
			return
		}
		if got, want := filter.Keep(code), filter.re.MatchString(code); got != want {
			t.Fatalf("keywords %q in %q: keep=%v, regex=%v", filter.Pattern(), code, got, want)
		}
	})
}
