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
