package jevtest

import (
	"context"
	"testing"

	"github.com/K1erans/jinter/jev"
)

var (
	_ jev.Client  = (*Fake)(nil)
	_ jev.Decider = (*Fake)(nil)
)

func TestFakeDecidesEveryQuestion(t *testing.T) {
	f := &Fake{Answer: func(state any, name string, q jev.Question) jev.Answer {
		switch q.Kind {
		case jev.Choice:
			return jev.Answer{Kind: jev.Choice, Choice: q.Options[0].Name}
		case jev.Score:
			return jev.Answer{Kind: jev.Score, Score: 1, LevelProbabilities: []float64{0, 1}}
		}
		return jev.Answer{Kind: jev.Noul, Noul: 0.5}
	}}
	q := jev.Query{Model: "jev-1.14.0", State: "func f() {}", Questions: map[string]jev.Question{
		"effects": jev.ChoiceQuestion("What does the `code` affect?", jev.Option{Name: "pure"}, jev.Option{Name: "writes"}),
		"risk":    jev.ScoreQuestion("How risky?", "safe", "risky"),
		"retries": jev.NoulQuestion("The `code` retries"),
	}}
	got, err := f.Decide(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "jev-1.14.0" || got.Answers["effects"].Choice != "pure" || got.Answers["risk"].Level() != 1 || got.Answers["retries"].Noul != 0.5 {
		t.Fatalf("got %+v", got)
	}
	if n := len(f.Queries()); n != 1 {
		t.Fatalf("%d queries recorded, want 1", n)
	}
	q.Questions["bad"] = jev.ChoiceQuestion("no options")
	if _, err := f.Decide(context.Background(), q); err == nil {
		t.Fatal("the fake accepted a query the real client would refuse")
	}
}
