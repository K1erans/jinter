package chunk

import (
	"context"
	"fmt"
	"sync"

	"github.com/K1erans/jinter/parse"
)

// Router is the router o.Parsers describes, or parse.Default() (which
// JINTER_PARSER sets) when it is empty. It fails when o.Parsers can't be
// used; Split then falls back to parse.Default().
func (o Options) Router() (*parse.Router, error) {
	if o.Parsers == "" {
		return parse.Default(), nil
	}
	if r, ok := routers.Load(o.Parsers); ok {
		return r.(*parse.Router), nil
	}
	r, err := parse.NewRouter(o.Parsers)
	if err != nil {
		return nil, fmt.Errorf("chunk: Options.Parsers: %w", err)
	}
	routers.Store(o.Parsers, r)
	return r, nil
}

var routers sync.Map // Options.Parsers → *parse.Router

func (o Options) router() *parse.Router {
	r, err := o.Router()
	if err != nil {
		return parse.Default()
	}
	return r
}

// String writes o as %+v would without Parsers, adding the routing only
// when some language leaves gotreesitter, so the default options print as
// they did before Parsers existed. search hashes this into its layout keys:
// layouts cached under the default routing stay valid, and each other
// routing gets its own.
func (o Options) String() string {
	s := fmt.Sprintf("{MinLines:%d MaxLines:%d MaxBytes:%d ParseTimeout:%v", o.MinLines, o.MaxLines, o.MaxBytes, o.ParseTimeout)
	if r := o.router().String(); r != "" {
		s += " Parsers:" + r
	}
	return s + "}"
}

// parseFile parses src with the backend o routes filename's language to.
// It fails, and Split falls back to the plain splitter, when there is no
// grammar, the parse stops early or times out, or the tree holds syntax
// errors.
func (o Options) parseFile(filename string, src []byte) (*parse.Tree, bool) {
	lang := parse.Detect(filename)
	if lang == "" {
		return nil, false
	}
	t, err := o.router().Parse(context.Background(), lang, src, o.ParseTimeout)
	if err != nil {
		return nil, false
	}
	if t.Root().HasError() {
		t.Release()
		return nil, false
	}
	return t, true
}

// hasNamedChild reports whether n has a named child.
func hasNamedChild(n parse.Node) bool {
	for i := range n.ChildCount() {
		if n.Child(i).IsNamed() {
			return true
		}
	}
	return false
}
