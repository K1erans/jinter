package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
)

// Decider asks typed questions about any state. It sits beside Client,
// which stays the search seam: Client asks Noul questions about chunks of a
// file, Decider asks any mix of Noul, Choice and Score questions about one
// state the caller builds. *HTTP and jevtest.Fake are both.
type Decider interface {
	Decide(ctx context.Context, q Query) (Answers, error)
}

// Kind is a question's type, as TypeSafe names it.
type Kind string

const (
	// Noul asks for the probability that a statement is true.
	Noul Kind = "noul"
	// Choice picks one of up to MaxOptions named options.
	Choice Kind = "choice"
	// Score places the state on an ordered rubric of up to MaxLevels levels.
	Score Kind = "score"
)

// TypeSafe's limits on one question.
const (
	MaxOptions = 255 // options in a Choice question
	MaxLevels  = 10  // levels in a Score question
)

// Query is one call to Jev: a state and named questions about it. Every
// question in a call shares the state, so the state is paid for once.
type Query struct {
	Model string
	// State is what every question refers to: a string, or anything
	// encoding/json writes as an object or array, such as
	// map[string]string{"a": codeA, "b": codeB}. Map keys are written in
	// sorted order, so the same state always encodes to the same bytes.
	State any
	// Questions are keyed by names the caller chooses; Answers come back
	// under the same names.
	Questions map[string]Question
}

// Question is one question about a Query's state. Build it with
// NoulQuestion, ChoiceQuestion or ScoreQuestion, or fill it in directly.
type Question struct {
	Kind Kind
	// Instructions are the question, or for a Noul a statement to judge.
	// Runs of white space are collapsed when sent, as Ask does.
	Instructions string
	// Options are a Choice question's options, in the order given. An
	// option with no Description is judged by its name alone.
	Options []Option
	// Levels are a Score question's rubric, from level 0 upwards: a
	// level's position is its score.
	Levels []string
}

// Option is one answer a Choice question can pick.
type Option struct {
	Name        string
	Description string // optional
}

// NoulQuestion asks for the probability that statement is true of the state.
func NoulQuestion(statement string) Question {
	return Question{Kind: Noul, Instructions: statement}
}

// ChoiceQuestion asks Jev to pick one of options.
func ChoiceQuestion(instructions string, options ...Option) Question {
	return Question{Kind: Choice, Instructions: instructions, Options: options}
}

// ScoreQuestion asks Jev to place the state on levels, lowest first.
func ScoreQuestion(instructions string, levels ...string) Question {
	return Question{Kind: Score, Instructions: instructions, Levels: levels}
}

// Answers are Jev's replies to one Query.
type Answers struct {
	Model   string            // the resolved version, e.g. "jev-1.14.0"
	Answers map[string]Answer // keyed by question name
}

// Answer is Jev's reply to one question. Which fields are set depends on
// Kind.
type Answer struct {
	Kind Kind

	// Noul: the probability of yes.
	Noul float64

	// Choice: the name of the most likely option.
	Choice string
	// Score: the expected level, the probability-weighted mean of the
	// levels, so it can fall between them (1.7 is mostly level 2).
	Score float64
	// Confidence is Jev's confidence in a Choice or Score answer, 0 to 1.
	Confidence float64
	// Probabilities of each option, by name, for Choice; of each level,
	// by level, for Score. They sum to about 1.
	Probabilities map[string]float64
	// LevelProbabilities is Score's Probabilities indexed by level.
	LevelProbabilities []float64
	// Legend is the rubric Jev scored against, indexed by level, as
	// TypeSafe returns it with a Score answer.
	Legend []string
}

// Level is the most likely level of a Score answer, or -1 for any other
// kind of answer.
func (a Answer) Level() int {
	if a.Kind != Score || len(a.LevelProbabilities) == 0 {
		return -1
	}
	best := 0
	for i, p := range a.LevelProbabilities {
		if p > a.LevelProbabilities[best] {
			best = i
		}
	}
	return best
}

// Validate checks q against TypeSafe's rules, so a malformed query fails
// before it costs a call.
func (q Query) Validate() error {
	if q.Model == "" {
		return errors.New("jev: query has no model")
	}
	if len(q.Questions) == 0 {
		return errors.New("jev: query has no questions")
	}
	for _, name := range slices.Sorted(maps.Keys(q.Questions)) {
		if name == "" {
			return errors.New("jev: a question has an empty name")
		}
		if err := q.Questions[name].validate(); err != nil {
			return fmt.Errorf("jev: question %q: %w", name, err)
		}
	}
	return nil
}

func (q Question) validate() error {
	switch q.Kind {
	case Noul:
		if len(q.Options) > 0 || len(q.Levels) > 0 {
			return errors.New("a noul question takes no options or levels")
		}
	case Choice:
		if len(q.Levels) > 0 {
			return errors.New("a choice question takes options, not levels")
		}
		if len(q.Options) == 0 || len(q.Options) > MaxOptions {
			return fmt.Errorf("a choice question needs 1 to %d options, has %d", MaxOptions, len(q.Options))
		}
		seen := make(map[string]bool, len(q.Options))
		for _, o := range q.Options {
			if o.Name == "" {
				return errors.New("an option has an empty name")
			}
			if seen[o.Name] {
				return fmt.Errorf("option %q appears twice", o.Name)
			}
			seen[o.Name] = true
		}
	case Score:
		if len(q.Options) > 0 {
			return errors.New("a score question takes levels, not options")
		}
		if len(q.Levels) == 0 || len(q.Levels) > MaxLevels {
			return fmt.Errorf("a score question needs 1 to %d levels, has %d", MaxLevels, len(q.Levels))
		}
	default:
		return fmt.Errorf("unknown question kind %q", q.Kind)
	}
	return nil
}

// Body is the request body Decide sends for q, after validating it. The
// same query always gives the same bytes, so a caller can key a cache on
// them: they are exactly what Jev saw.
func (q Query) Body() ([]byte, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	state, err := json.Marshal(q.State)
	if err != nil {
		return nil, fmt.Errorf("jev: encoding state: %w", err)
	}
	if string(state) == "null" {
		return nil, errors.New("jev: query has no state")
	}
	questions := make(map[string]wireQuestion, len(q.Questions))
	for name, qu := range q.Questions {
		questions[name] = qu.wire()
	}
	return json.Marshal(wireQuery{Model: q.Model, State: state, Questions: questions})
}

type wireQuery struct {
	Model     string                  `json:"model"`
	State     json.RawMessage         `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireQuestion struct {
	Type         Kind            `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

func (q Question) wire() wireQuestion {
	w := wireQuestion{Type: q.Kind, Instructions: NormalizeSpace(q.Instructions)}
	switch q.Kind {
	case Choice:
		w.Criteria = choiceCriteria(q.Options)
	case Score:
		w.Criteria, _ = json.Marshal(q.Levels) // can't fail for strings
	}
	return w
}

// choiceCriteria writes options as a JSON object in the order given, with
// null for an option with no description, as TypeSafe's examples do. A Go
// map would sort them; keeping the caller's order keeps the body the same
// as the question reads.
func choiceCriteria(options []Option) json.RawMessage {
	b := []byte{'{'}
	for i, o := range options {
		if i > 0 {
			b = append(b, ',')
		}
		name, _ := json.Marshal(o.Name)
		b = append(b, name...)
		b = append(b, ':')
		if o.Description == "" {
			b = append(b, "null"...)
		} else {
			desc, _ := json.Marshal(o.Description)
			b = append(b, desc...)
		}
	}
	return append(b, '}')
}

type wireAnswers struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
}

type wireAnswer struct {
	Type          Kind               `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        *string            `json:"choice"`
	Score         *float64           `json:"score"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Legend        map[string]string  `json:"legend"`
}

// decodeAnswers reads a reply to q, checking every question was answered
// with its own kind.
func decodeAnswers(q Query, raw []byte) (Answers, error) {
	var w wireAnswers
	if err := json.Unmarshal(raw, &w); err != nil {
		return Answers{}, fmt.Errorf("reading TypeSafe response: %w", err)
	}
	out := Answers{Model: w.Model, Answers: make(map[string]Answer, len(q.Questions))}
	for _, name := range slices.Sorted(maps.Keys(q.Questions)) {
		qu := q.Questions[name]
		wa, ok := w.Answers[name]
		if !ok {
			return Answers{}, fmt.Errorf("TypeSafe response has no answer for %q", name)
		}
		a, err := wa.answer(qu)
		if err != nil {
			return Answers{}, fmt.Errorf("TypeSafe answer for %q: %w", name, err)
		}
		out.Answers[name] = a
	}
	return out, nil
}

func (w wireAnswer) answer(q Question) (Answer, error) {
	if w.Type != q.Kind {
		return Answer{}, fmt.Errorf("is a %q answer to a %q question", w.Type, q.Kind)
	}
	a := Answer{Kind: q.Kind, Probabilities: w.Probabilities}
	switch q.Kind {
	case Noul:
		if w.Noul == nil {
			return Answer{}, errors.New("has no noul")
		}
		a.Noul = *w.Noul
		return a, nil
	case Choice:
		if w.Choice == nil {
			return Answer{}, errors.New("has no choice")
		}
		if !slices.ContainsFunc(q.Options, func(o Option) bool { return o.Name == *w.Choice }) {
			return Answer{}, fmt.Errorf("chose %q, which is not an option", *w.Choice)
		}
		a.Choice = *w.Choice
	case Score:
		if w.Score == nil {
			return Answer{}, errors.New("has no score")
		}
		a.Score = *w.Score
		a.LevelProbabilities = make([]float64, len(q.Levels))
		for k, p := range w.Probabilities {
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(q.Levels) {
				return Answer{}, fmt.Errorf("has a probability for level %q of %d", k, len(q.Levels))
			}
			a.LevelProbabilities[i] = p
		}
		a.Legend = legend(w.Legend, q.Levels)
	}
	if w.Confidence != nil {
		a.Confidence = *w.Confidence
	}
	return a, nil
}

// legend orders TypeSafe's legend, keyed "0", "1", …, by level. A level
// it leaves out reads as the level the caller sent.
func legend(w map[string]string, levels []string) []string {
	out := slices.Clone(levels)
	for k, v := range w {
		if i, err := strconv.Atoi(k); err == nil && i >= 0 && i < len(out) {
			out[i] = v
		}
	}
	return out
}
