package parse

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/odvcencio/gotreesitter/grammars"
)

// EnvVar routes languages to backends for Default, in Router's spec
// format, e.g. JINTER_PARSER="typescript=c,tsx=c,javascript=c".
const EnvVar = "JINTER_PARSER"

// backends are the backends this build has, by name.
var backends = map[string]Backend{"gotreesitter": gts}

// Backends lists the backends this build has: gotreesitter always, c with
// the jinter_cgo build tag.
func Backends() []Backend {
	var out []Backend
	for _, name := range slices.Sorted(maps.Keys(backends)) {
		out = append(out, backends[name])
	}
	return out
}

// Lookup finds a backend by name, or nil.
func Lookup(name string) Backend { return backends[name] }

// Detect names the language of filename, by gotreesitter's registry, which
// every backend's language names follow; "" when there is none.
func Detect(filename string) string {
	if e := grammars.DetectLanguage(filename); e != nil {
		return e.Name
	}
	return ""
}

// Router picks a backend per language. It is a Backend itself: it parses
// each language with the backend routed to it, and any language that
// backend can't parse with gotreesitter.
type Router struct {
	fallback Backend            // for "*"
	byLang   map[string]Backend // explicit routes
	spec     string
}

// NewRouter routes by spec: comma-separated lang=backend pairs, with "*"
// for every other language, e.g. "typescript=c,tsx=c" or "*=c". Languages
// with no route go to gotreesitter, and so does any language a "*" backend
// can't parse. An empty spec routes everything to gotreesitter.
func NewRouter(spec string) (*Router, error) {
	r := &Router{fallback: gts, byLang: map[string]Backend{}}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lang, name, ok := strings.Cut(part, "=")
		lang, name = strings.TrimSpace(lang), strings.TrimSpace(name)
		if !ok || lang == "" || name == "" {
			return nil, fmt.Errorf("parse: route %q is not lang=backend", part)
		}
		b := backends[name]
		if b == nil {
			return nil, fmt.Errorf("parse: no backend %q in this build (have %s)", name, strings.Join(slices.Sorted(maps.Keys(backends)), ", "))
		}
		if lang == "*" {
			r.fallback = b
			continue
		}
		if !b.Supports(lang) {
			return nil, fmt.Errorf("parse: backend %s can't parse %q (it parses %s)", name, lang, strings.Join(b.Languages(), ", "))
		}
		r.byLang[lang] = b
	}
	r.spec = r.canonical()
	return r, nil
}

// canonical writes the routes sorted, leaving out gotreesitter's, so equal
// routings give equal specs and the default gives "".
func (r *Router) canonical() string {
	var parts []string
	for _, lang := range slices.Sorted(maps.Keys(r.byLang)) {
		if b := r.byLang[lang]; b != gts {
			parts = append(parts, lang+"="+b.Name())
		}
	}
	if r.fallback != gts {
		parts = append(parts, "*="+r.fallback.Name())
	}
	return strings.Join(parts, ",")
}

// String is the routing in NewRouter's format, sorted and without
// gotreesitter's routes: "" means everything goes to gotreesitter. Two
// routers with the same String parse every file the same way.
func (r *Router) String() string { return r.spec }

// For is the backend that parses lang.
func (r *Router) For(lang string) Backend {
	if b, ok := r.byLang[lang]; ok {
		return b
	}
	if r.fallback.Supports(lang) {
		return r.fallback
	}
	return gts
}

// Name is "router" followed by the routing.
func (r *Router) Name() string { return "router(" + r.spec + ")" }

// Languages are every language some backend parses, which gotreesitter
// alone covers.
func (r *Router) Languages() []string { return gts.Languages() }

// Supports says whether the routed backend, or gotreesitter, parses lang.
func (r *Router) Supports(lang string) bool { return r.For(lang).Supports(lang) }

// Parse parses src as lang with the backend routed to lang.
func (r *Router) Parse(ctx context.Context, lang string, src []byte, timeout time.Duration) (*Tree, error) {
	return r.For(lang).Parse(ctx, lang, src, timeout)
}

// ParseFile parses src as the language of filename (see Detect).
func (r *Router) ParseFile(ctx context.Context, filename string, src []byte, timeout time.Duration) (*Tree, error) {
	lang := Detect(filename)
	if lang == "" {
		return nil, fmt.Errorf("%s: %w", filename, ErrUnsupported)
	}
	t, err := r.Parse(ctx, lang, src, timeout)
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", filename, lang, err)
	}
	return t, nil
}

var defaultRouter = sync.OnceValues(func() (*Router, error) {
	return NewRouter(os.Getenv(EnvVar))
})

// Default is the router JINTER_PARSER describes, read once. When the
// variable is unset it routes everything to gotreesitter. When it can't be
// used, Default warns once on stderr and routes everything to gotreesitter;
// DefaultErr returns the reason.
func Default() *Router {
	r, err := defaultRouter()
	if err != nil {
		warnOnce.Do(func() { fmt.Fprintf(os.Stderr, "jinter: ignoring %s: %v\n", EnvVar, err) })
		return gtsOnly
	}
	return r
}

// DefaultErr is why Default couldn't use JINTER_PARSER, or nil.
func DefaultErr() error {
	_, err := defaultRouter()
	return err
}

var (
	warnOnce sync.Once
	gtsOnly  = &Router{fallback: gts, byLang: map[string]Backend{}}
)
