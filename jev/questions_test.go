package jev

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

var _ Decider = (*HTTP)(nil)

// judge is a query of every kind about a pair of functions, as Scruple's
// Judge asks.
var judge = Query{
	Model: "jev-1.14.0",
	State: map[string]string{"b": "func g() {}", "a": "func f() {}"},
	Questions: map[string]Question{
		"same_behaviour": NoulQuestion("`a` and `b` do the same job   for their caller"),
		"consolidation": ChoiceQuestion("How should `a` and `b` be consolidated?",
			Option{Name: "extract shared function"},
			Option{Name: "parameterize", Description: "one function with a parameter for what differs"},
			Option{Name: "leave alone"}),
		"risk": ScoreQuestion("How risky is replacing `b` with `a`?",
			"safe", "needs tests", "behaviour may differ"),
	},
}

// judgeBody is judge as TypeSafe's API reference writes such a request:
// choice criteria an object of option names to descriptions (null for
// none), score criteria an array, lowest level first.
const judgeBody = `{"model":"jev-1.14.0",` +
	`"state":{"a":"func f() {}","b":"func g() {}"},` +
	`"questions":{` +
	`"consolidation":{"type":"choice","instructions":"How should ` + "`a` and `b`" + ` be consolidated?",` +
	`"criteria":{"extract shared function":null,"parameterize":"one function with a parameter for what differs","leave alone":null}},` +
	`"risk":{"type":"score","instructions":"How risky is replacing ` + "`b` with `a`" + `?",` +
	`"criteria":["safe","needs tests","behaviour may differ"]},` +
	`"same_behaviour":{"type":"noul","instructions":"` + "`a` and `b`" + ` do the same job for their caller"}}}`

// judgeReply answers judge in the shapes of TypeSafe's API reference.
const judgeReply = `{"model":"jev-1.14.0","answers":{` +
	`"same_behaviour":{"type":"noul","noul":0.91},` +
	`"consolidation":{"type":"choice","choice":"parameterize","confidence":0.7,` +
	`"probabilities":{"extract shared function":0.15,"parameterize":0.8,"leave alone":0.05}},` +
	`"risk":{"type":"score","score":0.4,"confidence":0.55,` +
	`"legend":{"0":"safe","1":"needs tests","2":"behaviour may differ"},` +
	`"probabilities":{"0":0.7,"1":0.2,"2":0.1}}},` +
	`"usage":{"input_tokens":350,"output_tokens":30}}`

func TestQueryBodyIsTheDocumentedRequest(t *testing.T) {
	got, err := judge.Body()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != judgeBody {
		t.Fatalf("body\n got  %s\n want %s", got, judgeBody)
	}
	if !json.Valid(got) {
		t.Fatal("body is not valid JSON")
	}
	// The same query always encodes the same, so its bytes can key a cache.
	for range 20 {
		if again, _ := judge.Body(); string(again) != string(got) {
			t.Fatalf("body changed between calls:\n%s\n%s", got, again)
		}
	}
}

func TestStatesCanBeStrings(t *testing.T) {
	q := Query{Model: "jev-latest", State: "func f() {}", Questions: map[string]Question{"q": NoulQuestion("x")}}
	got, err := q.Body()
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"model":"jev-latest","state":"func f() {}","questions":{"q":{"type":"noul","instructions":"x"}}}`; string(got) != want {
		t.Fatalf("body %s, want %s", got, want)
	}
}

func TestDecidesAMixedRequest(t *testing.T) {
	c, _, calls := serve(t, quick, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("%s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if body, _ := io.ReadAll(r.Body); string(body) != judgeBody {
			t.Errorf("body %s", body)
		}
		io.WriteString(w, judgeReply)
	})
	got, err := c.Decide(context.Background(), judge)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("%d calls, want 1 for every question", calls.Load())
	}
	if got.Model != "jev-1.14.0" || len(got.Answers) != 3 {
		t.Fatalf("got %+v", got)
	}

	if a := got.Answers["same_behaviour"]; a.Kind != Noul || a.Noul != 0.91 {
		t.Errorf("noul answer %+v", a)
	}

	ch := got.Answers["consolidation"]
	if ch.Kind != Choice || ch.Choice != "parameterize" || ch.Confidence != 0.7 ||
		ch.Probabilities["parameterize"] != 0.8 || ch.Probabilities["leave alone"] != 0.05 || len(ch.Probabilities) != 3 {
		t.Errorf("choice answer %+v", ch)
	}
	if ch.Level() != -1 {
		t.Errorf("a choice answer has level %d, want -1", ch.Level())
	}

	sc := got.Answers["risk"]
	if sc.Kind != Score || sc.Score != 0.4 || sc.Confidence != 0.55 {
		t.Errorf("score answer %+v", sc)
	}
	if want := []float64{0.7, 0.2, 0.1}; !slices.Equal(sc.LevelProbabilities, want) {
		t.Errorf("level probabilities %v, want %v", sc.LevelProbabilities, want)
	}
	if want := []string{"safe", "needs tests", "behaviour may differ"}; !slices.Equal(sc.Legend, want) {
		t.Errorf("legend %q, want %q", sc.Legend, want)
	}
	if sc.Level() != 0 {
		t.Errorf("most likely level %d, want 0", sc.Level())
	}
}

func TestBadAnswersAreErrors(t *testing.T) {
	cases := map[string]string{
		"missing answer":     `{"model":"m","answers":{"same_behaviour":{"type":"noul","noul":0.9},"consolidation":{"type":"choice","choice":"parameterize"}}}`,
		"wrong kind":         strings.Replace(judgeReply, `"risk":{"type":"score"`, `"risk":{"type":"choice"`, 1),
		"not an option":      strings.Replace(judgeReply, `"choice":"parameterize"`, `"choice":"rewrite it"`, 1),
		"no choice":          strings.Replace(judgeReply, `"choice":"parameterize",`, ``, 1),
		"no score":           strings.Replace(judgeReply, `"score":0.4,`, ``, 1),
		"no noul":            strings.Replace(judgeReply, `"noul":0.91`, `"nouls":0.91`, 1),
		"level out of range": strings.Replace(judgeReply, `"2":0.1}}}`, `"3":0.1}}}`, 1),
		"not JSON":           `<html>`,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			opts := quick
			opts.MaxRetries = -1
			c, _, _ := serve(t, opts, func(w http.ResponseWriter, _ *http.Request, _ int) { io.WriteString(w, reply) })
			if got, err := c.Decide(context.Background(), judge); err == nil {
				t.Fatalf("no error; got %+v", got)
			}
		})
	}
}

// A bad reply is retried, as Ask retries one.
func TestBadAnswersAreRetried(t *testing.T) {
	c, _, calls := serve(t, quick, func(w http.ResponseWriter, _ *http.Request, call int) {
		if call == 1 {
			io.WriteString(w, `{"model":"m","answers":{}}`)
			return
		}
		io.WriteString(w, judgeReply)
	})
	if _, err := c.Decide(context.Background(), judge); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("%d calls, want 2", calls.Load())
	}
}

func TestInvalidQueriesFailBeforeACall(t *testing.T) {
	many := make([]Option, MaxOptions+1)
	for i := range many {
		many[i] = Option{Name: strings.Repeat("x", i+1)}
	}
	q := func(qs ...Question) Query {
		m := map[string]Question{}
		for i, qu := range qs {
			m[string(rune('a'+i))] = qu
		}
		return Query{Model: "jev-latest", State: "s", Questions: m}
	}
	cases := map[string]Query{
		"no model":              {State: "s", Questions: map[string]Question{"a": NoulQuestion("x")}},
		"no state":              {Model: "jev-latest", Questions: map[string]Question{"a": NoulQuestion("x")}},
		"no questions":          {Model: "jev-latest", State: "s"},
		"empty question name":   {Model: "jev-latest", State: "s", Questions: map[string]Question{"": NoulQuestion("x")}},
		"unknown kind":          q(Question{Kind: "box", Instructions: "x"}),
		"choice of nothing":     q(ChoiceQuestion("x")),
		"choice of 256":         q(ChoiceQuestion("x", many...)),
		"option named twice":    q(ChoiceQuestion("x", Option{Name: "a"}, Option{Name: "a"})),
		"option with no name":   q(ChoiceQuestion("x", Option{Name: ""})),
		"choice with levels":    q(Question{Kind: Choice, Options: []Option{{Name: "a"}}, Levels: []string{"l"}}),
		"score of nothing":      q(ScoreQuestion("x")),
		"score of 11":           q(ScoreQuestion("x", "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10")),
		"score with options":    q(Question{Kind: Score, Levels: []string{"l"}, Options: []Option{{Name: "a"}}}),
		"noul with options":     q(Question{Kind: Noul, Options: []Option{{Name: "a"}}}),
		"state JSON can't hold": {Model: "jev-latest", State: func() {}, Questions: map[string]Question{"a": NoulQuestion("x")}},
	}
	c, _, calls := serve(t, quick, func(w http.ResponseWriter, _ *http.Request, _ int) { io.WriteString(w, judgeReply) })
	for name, query := range cases {
		if _, err := c.Decide(context.Background(), query); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d calls for invalid queries, want 0", calls.Load())
	}
	// The limits themselves are allowed.
	if _, err := q(ChoiceQuestion("x", many[:MaxOptions]...), ScoreQuestion("x", "0", "1", "2", "3", "4", "5", "6", "7", "8", "9")).Body(); err != nil {
		t.Errorf("255 options and 10 levels: %v", err)
	}
}

// TestRealAPIDecideSmoke asks TypeSafe every kind of question at once, with
// a pinned model version. It runs only with a key set.
func TestRealAPIDecideSmoke(t *testing.T) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		t.Skip("TYPESAFE_API_KEY not set")
	}
	c := NewHTTP(key, HTTPOptions{BaseURL: os.Getenv("TYPESAFE_BASE_URL")})
	q := judge
	q.State = map[string]string{
		"a": "func round2(x float64) float64 { return math.Round(x*100) / 100 }",
		"b": "func roundCents(v float64) float64 { return math.Round(v*100) / 100 }",
	}
	got, err := c.Decide(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", got)
	if got.Model != q.Model {
		t.Errorf("asked for %s, answered by %s", q.Model, got.Model)
	}
	if got.Answers["same_behaviour"].Noul < 0.5 || got.Answers["risk"].Level() != 0 {
		t.Errorf("unexpected answers %+v", got)
	}
}
