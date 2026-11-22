package jinter_test

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/K1erans/jinter"
	"github.com/K1erans/jinter/jev"
	"github.com/K1erans/jinter/search"
)

// These examples have no expected output, so they are compiled but never
// run: they keep the README's code in step with the API.

// A search, as in the README's quick start.
func Example() {
	engine := jinter.New(os.Getenv("TYPESAFE_API_KEY"), jev.HTTPOptions{})
	rep, err := engine.Search(context.Background(), jinter.Query{
		Questions: []string{"retries failed payments", "sends the receipt email"},
		Paths:     []string{"src/billing"},
		Threshold: 0.6,
		Top:       10,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range rep.Results {
		fmt.Println("#", r.Question)
		for _, m := range r.Matches {
			fmt.Printf("%.2f  %s:%d-%d  %s\n", m.Probability, m.Path, m.Start, m.End, m.Signature)
		}
	}
}

// Showing matches while the search is still running.
func ExampleQuery_onMatch() {
	engine := jinter.New(os.Getenv("TYPESAFE_API_KEY"), jev.HTTPOptions{})
	rep, err := engine.Search(context.Background(), jinter.Query{
		Questions: []string{"retries failed payments"},
		Threshold: 0.6,
		// Called as each answer comes in, in no order of rank.
		OnMatch: func(question string, m search.Match) {
			fmt.Printf("found %.2f  %s:%d-%d\n", m.Probability, m.Path, m.Start, m.End)
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	// The report has every match, best first.
	for _, m := range rep.Results[0].Matches {
		fmt.Printf("%.2f  %s:%d-%d  %s\n", m.Probability, m.Path, m.Start, m.End, m.Signature)
	}
}
