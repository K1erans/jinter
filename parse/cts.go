//go:build jinter_cgo

package parse

/*
#cgo CFLAGS: -O2 -std=c11 -D_POSIX_C_SOURCE=200112L

// The types and functions of tree_sitter/api.h (v0.25) this file uses,
// declared here because the header lives in go-tree-sitter's module. The
// functions come from go-tree-sitter's copy of the C runtime, linked in by
// importing it.

#include <stdbool.h>
#include <stdint.h>
#include <stdlib.h>
#include <time.h>

typedef uint16_t TSSymbol;
typedef uint16_t TSFieldId;
typedef struct TSLanguage TSLanguage;
typedef struct TSParser TSParser;
typedef struct TSTree TSTree;
typedef struct TSQuery TSQuery;
typedef struct TSQueryCursor TSQueryCursor;

typedef uint32_t (*DecodeFunction)(const uint8_t *string, uint32_t length, int32_t *code_point);
typedef enum { TSInputEncodingUTF8, TSInputEncodingUTF16LE, TSInputEncodingUTF16BE, TSInputEncodingCustom } TSInputEncoding;
typedef struct { uint32_t row; uint32_t column; } TSPoint;
typedef struct {
	void *payload;
	const char *(*read)(void *payload, uint32_t byte_index, TSPoint position, uint32_t *bytes_read);
	TSInputEncoding encoding;
	DecodeFunction decode;
} TSInput;
typedef struct { void *payload; uint32_t current_byte_offset; bool has_error; } TSParseState;
typedef struct { void *payload; bool (*progress_callback)(TSParseState *state); } TSParseOptions;
typedef struct { uint32_t context[4]; const void *id; const TSTree *tree; } TSNode;
typedef struct { const void *tree; const void *id; uint32_t context[3]; } TSTreeCursor;
typedef struct { TSNode node; uint32_t index; } TSQueryCapture;
typedef struct { uint32_t id; uint16_t pattern_index; uint16_t capture_count; const TSQueryCapture *captures; } TSQueryMatch;

TSParser *ts_parser_new(void);
bool ts_parser_set_language(TSParser *self, const TSLanguage *language);
TSTree *ts_parser_parse_with_options(TSParser *self, const TSTree *old_tree, TSInput input, TSParseOptions parse_options);
void ts_parser_reset(TSParser *self);
TSNode ts_tree_root_node(const TSTree *self);
void ts_tree_delete(TSTree *self);

TSSymbol ts_node_symbol(TSNode self);
bool ts_node_is_null(TSNode self);
bool ts_node_is_named(TSNode self);
bool ts_node_is_missing(TSNode self);
bool ts_node_is_error(TSNode self);
bool ts_node_has_error(TSNode self);
uint32_t ts_node_start_byte(TSNode self);
uint32_t ts_node_end_byte(TSNode self);
TSPoint ts_node_start_point(TSNode self);
TSPoint ts_node_end_point(TSNode self);
uint32_t ts_node_child_count(TSNode self);
TSNode ts_node_child(TSNode self, uint32_t child_index);
TSNode ts_node_parent(TSNode self);
TSNode ts_node_child_by_field_id(TSNode self, TSFieldId field_id);

TSTreeCursor ts_tree_cursor_new(TSNode node);
void ts_tree_cursor_delete(TSTreeCursor *self);
void ts_tree_cursor_reset(TSTreeCursor *self, TSNode node);
bool ts_tree_cursor_goto_first_child(TSTreeCursor *self);
bool ts_tree_cursor_goto_next_sibling(TSTreeCursor *self);
TSNode ts_tree_cursor_current_node(const TSTreeCursor *self);
TSFieldId ts_tree_cursor_current_field_id(const TSTreeCursor *self);

uint32_t ts_language_symbol_count(const TSLanguage *self);
const char *ts_language_symbol_name(const TSLanguage *self, TSSymbol symbol);
uint32_t ts_language_field_count(const TSLanguage *self);
const char *ts_language_field_name_for_id(const TSLanguage *self, TSFieldId id);

TSQueryCursor *ts_query_cursor_new(void);
void ts_query_cursor_delete(TSQueryCursor *self);
void ts_query_cursor_exec(TSQueryCursor *self, const TSQuery *query, TSNode node);
bool ts_query_cursor_next_capture(TSQueryCursor *self, TSQueryMatch *match, uint32_t *capture_index);
void ts_query_cursor_remove_match(TSQueryCursor *self, uint32_t match_id);

// jt_input is a parse's source and limits.
typedef struct {
	const char *src;
	uint32_t len;
	bool has_deadline;
	struct timespec deadline;
	volatile int32_t *cancel;
	int stopped; // why the parse stopped: 1 deadline, 2 cancelled
} jt_input;

static const char *jt_read(void *payload, uint32_t at, TSPoint pos, uint32_t *n) {
	jt_input *in = payload;
	if (at >= in->len) {
		*n = 0;
		return "";
	}
	*n = in->len - at;
	return in->src + at;
}

static bool jt_progress(TSParseState *state) {
	jt_input *in = state->payload;
	if (in->cancel != NULL && __atomic_load_n(in->cancel, __ATOMIC_RELAXED)) {
		in->stopped = 2;
		return true;
	}
	if (in->has_deadline) {
		struct timespec now;
		clock_gettime(CLOCK_MONOTONIC, &now);
		if (now.tv_sec > in->deadline.tv_sec || (now.tv_sec == in->deadline.tv_sec && now.tv_nsec >= in->deadline.tv_nsec)) {
			in->stopped = 1;
			return true;
		}
	}
	return false;
}

// jt_parse parses src[:len] with p, giving up after timeout_ns (0: never)
// or once *cancel is set. With no tree, *stopped says why.
static TSTree *jt_parse(TSParser *p, const char *src, uint32_t len, uint64_t timeout_ns, volatile int32_t *cancel, int *stopped) {
	jt_input in = {src, len, false, {0, 0}, cancel, 0};
	TSParseOptions opts = {NULL, NULL};
	if (timeout_ns > 0 || cancel != NULL) {
		opts.payload = &in;
		opts.progress_callback = jt_progress;
	}
	if (timeout_ns > 0) {
		clock_gettime(CLOCK_MONOTONIC, &in.deadline);
		uint64_t ns = (uint64_t)in.deadline.tv_nsec + timeout_ns;
		in.deadline.tv_sec += ns / 1000000000u;
		in.deadline.tv_nsec = ns % 1000000000u;
		in.has_deadline = true;
	}
	TSInput input = {&in, jt_read, TSInputEncodingUTF8, NULL};
	TSTree *t = ts_parser_parse_with_options(p, NULL, input, opts);
	if (t == NULL) {
		ts_parser_reset(p);
	}
	*stopped = in.stopped;
	return t;
}

// jt_children writes n's children, or its named children, to out, up to
// cap of them, and returns how many n has. One cursor walk is linear;
// ts_node_child(i) counts from the first child on every call.
static uint32_t jt_children(TSTreeCursor *c, TSNode n, bool named_only, TSNode *out, uint32_t cap) {
	uint32_t k = 0;
	ts_tree_cursor_reset(c, n);
	if (!ts_tree_cursor_goto_first_child(c)) {
		return 0;
	}
	do {
		TSNode child = ts_tree_cursor_current_node(c);
		if (named_only && !ts_node_is_named(child)) {
			continue;
		}
		if (k < cap) {
			out[k] = child;
		}
		k++;
	} while (ts_tree_cursor_goto_next_sibling(c));
	return k;
}

// jt_field is the field id under which parent holds the child whose id
// is id, or 0.
static TSFieldId jt_field(TSTreeCursor *c, TSNode parent, const void *id) {
	ts_tree_cursor_reset(c, parent);
	if (!ts_tree_cursor_goto_first_child(c)) {
		return 0;
	}
	do {
		if (ts_tree_cursor_current_node(c).id == id) {
			return ts_tree_cursor_current_field_id(c);
		}
	} while (ts_tree_cursor_goto_next_sibling(c));
	return 0;
}

static TSTreeCursor *jt_cursor_new(TSNode n) {
	TSTreeCursor *c = malloc(sizeof *c);
	*c = ts_tree_cursor_new(n);
	return c;
}

static void jt_cursor_delete(TSTreeCursor *c) {
	ts_tree_cursor_delete(c);
	free(c);
}

// jt_next_capture is ts_query_cursor_next_capture, with the match copied
// to *m (the captures stay in the cursor's memory). go-tree-sitter's own
// loop mallocs a TSQueryMatch per capture and never frees it.
static bool jt_next_capture(TSQueryCursor *c, TSQueryMatch *m, uint32_t *index) {
	return ts_query_cursor_next_capture(c, m, index);
}
*/
import "C"

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	sitter "github.com/tree-sitter/go-tree-sitter"
	tsc "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tscpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tshtml "github.com/tree-sitter/tree-sitter-html/bindings/go"
	tsjava "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tsjson "github.com/tree-sitter/tree-sitter-json/bindings/go"
	tspython "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tsruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	tsrust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// The grammars' own highlights.scm files, at the versions go.mod pins (see
// queries/NOTICE).
var (
	//go:embed queries/javascript.scm
	qJavaScript string
	//go:embed queries/jsx.scm
	qJSX string
	//go:embed queries/typescript.scm
	qTypeScript string
	//go:embed queries/go.scm
	qGo string
	//go:embed queries/python.scm
	qPython string
	//go:embed queries/rust.scm
	qRust string
	//go:embed queries/c.scm
	qC string
	//go:embed queries/cpp.scm
	qCpp string
	//go:embed queries/java.scm
	qJava string
	//go:embed queries/ruby.scm
	qRuby string
	//go:embed queries/json.scm
	qJSON string
	//go:embed queries/html.scm
	qHTML string
)

// ctsGrammars are the C grammars, by gotreesitter's names for them, with
// their highlight queries put together as each grammar's tree-sitter.json
// lists them (earlier patterns win).
var ctsGrammars = map[string]struct {
	lang      func() unsafe.Pointer
	highlight []*string
}{
	"javascript": {tsjs.Language, []*string{&qJavaScript, &qJSX}},
	"typescript": {tsts.LanguageTypescript, []*string{&qTypeScript, &qJavaScript}},
	"tsx":        {tsts.LanguageTSX, []*string{&qTypeScript, &qJSX, &qJavaScript}},
	"go":         {tsgo.Language, []*string{&qGo}},
	"python":     {tspython.Language, []*string{&qPython}},
	"rust":       {tsrust.Language, []*string{&qRust}},
	"c":          {tsc.Language, []*string{&qC}},
	"cpp":        {tscpp.Language, []*string{&qC, &qCpp}},
	"java":       {tsjava.Language, []*string{&qJava}},
	"ruby":       {tsruby.Language, []*string{&qRuby}},
	"json":       {tsjson.Language, []*string{&qJSON}},
	"html":       {tshtml.Language, []*string{&qHTML}},
}

func init() { backends["c"] = cts }

// CTreeSitter is the C tree-sitter runtime (through cgo) with the official
// grammars. It is in builds with the jinter_cgo tag only.
var CTreeSitter Backend = cts

var cts = &ctsBackend{langs: map[string]*ctsLang{}}

type ctsBackend struct {
	mu    sync.Mutex
	langs map[string]*ctsLang
}

// ctsLang is one grammar: its tables, made once so node types and field
// names cost no C call or allocation, and its idle parsers.
type ctsLang struct {
	name    string
	lang    *C.TSLanguage
	sitter  *sitter.Language
	symbols []string // by symbol id
	fields  []string // by field id; 0 is ""
	fieldID map[string]C.TSFieldId

	mu   sync.Mutex
	idle []*ctsParser

	query     sync.Once
	q         *sitter.Query
	qCaptures []string
	qErr      error
}

// ctsParser is a C parser with its cancellation flag, which is in C memory
// so the C progress callback can read it.
type ctsParser struct {
	p      *C.TSParser
	cancel *C.int32_t
}

func (*ctsBackend) Name() string { return "c" }

func (*ctsBackend) Languages() []string {
	var out []string
	for name := range ctsGrammars {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func (*ctsBackend) Supports(lang string) bool {
	_, ok := ctsGrammars[lang]
	return ok
}

// language loads lang's grammar and tables on first use.
func (b *ctsBackend) language(name string) *ctsLang {
	b.mu.Lock()
	defer b.mu.Unlock()
	if l := b.langs[name]; l != nil {
		return l
	}
	g, ok := ctsGrammars[name]
	if !ok {
		return nil
	}
	ptr := g.lang()
	cl := (*C.TSLanguage)(ptr)
	l := &ctsLang{name: name, lang: cl, sitter: sitter.NewLanguage(ptr), fieldID: map[string]C.TSFieldId{}}
	n := int(C.ts_language_symbol_count(cl))
	l.symbols = make([]string, n)
	for i := range n {
		l.symbols[i] = C.GoString(C.ts_language_symbol_name(cl, C.TSSymbol(i)))
	}
	nf := int(C.ts_language_field_count(cl))
	l.fields = make([]string, nf+1)
	for i := 1; i <= nf; i++ {
		f := C.GoString(C.ts_language_field_name_for_id(cl, C.TSFieldId(i)))
		l.fields[i] = f
		l.fieldID[f] = C.TSFieldId(i)
	}
	b.langs[name] = l
	return l
}

func (l *ctsLang) symbol(s C.TSSymbol) string {
	switch {
	case int(s) < len(l.symbols):
		return l.symbols[s]
	case s == 0xFFFF:
		return "ERROR"
	case s == 0xFFFE:
		return "_ERROR"
	}
	return ""
}

func (l *ctsLang) get() *ctsParser {
	l.mu.Lock()
	if n := len(l.idle); n > 0 {
		p := l.idle[n-1]
		l.idle = l.idle[:n-1]
		l.mu.Unlock()
		return p
	}
	l.mu.Unlock()
	p := &ctsParser{p: C.ts_parser_new(), cancel: (*C.int32_t)(C.calloc(1, 4))}
	if !C.ts_parser_set_language(p.p, l.lang) {
		panic(fmt.Sprintf("parse: C tree-sitter refuses the %s grammar's ABI", l.name))
	}
	return p
}

func (l *ctsLang) put(p *ctsParser) {
	l.mu.Lock()
	l.idle = append(l.idle, p)
	l.mu.Unlock()
}

func (b *ctsBackend) Parse(ctx context.Context, lang string, src []byte, timeout time.Duration) (*Tree, error) {
	l := b.language(lang)
	if l == nil {
		return nil, fmt.Errorf("%w: C tree-sitter has no %q grammar in this build", ErrUnsupported, lang)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCancelled, err)
	}
	if uint64(len(src)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("%w: %d bytes is too long for C tree-sitter", ErrStoppedEarly, len(src))
	}
	p := l.get()
	defer l.put(p)

	var cancel *C.int32_t
	if ctx.Done() != nil {
		cancel = p.cancel
		atomic.StoreInt32((*int32)(unsafe.Pointer(cancel)), 0)
		done := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			atomic.StoreInt32((*int32)(unsafe.Pointer(cancel)), 1)
			close(done)
		})
		defer func() {
			if !stop() {
				<-done // the flag is written before p parses again
			}
		}()
	}
	var srcPtr *C.char
	if len(src) > 0 {
		srcPtr = (*C.char)(unsafe.Pointer(&src[0]))
	}
	var stopped C.int
	if timeout < 0 {
		timeout = 0
	}
	ct := C.jt_parse(p.p, srcPtr, C.uint32_t(len(src)), C.uint64_t(timeout), cancel, &stopped)
	if ct == nil {
		switch stopped {
		case 1:
			return nil, fmt.Errorf("%w after %v", ErrTimeout, timeout)
		case 2:
			return nil, ErrCancelled
		}
		return nil, fmt.Errorf("%w: C tree-sitter returned no tree", ErrStoppedEarly)
	}
	root := C.ts_tree_root_node(ct)
	t := &ctsTree{t: ct, lang: l, cursor: C.jt_cursor_new(root)}
	return &Tree{t: t, root: t.node(root), lang: lang, backend: "c", src: src}, nil
}

type ctsTree struct {
	t      *C.TSTree
	lang   *ctsLang
	cursor *C.TSTreeCursor
	buf    *C.TSNode // C scratch for children, of bufCap nodes
	bufCap int
}

// A Node holds a TSNode in raw: they are both 32 bytes.
var _ [unsafe.Sizeof(Node{}.raw) - unsafe.Sizeof(C.TSNode{})]byte
var _ [unsafe.Sizeof(C.TSNode{}) - unsafe.Sizeof(Node{}.raw)]byte

func (t *ctsTree) node(n C.TSNode) Node {
	if n.id == nil {
		return Node{}
	}
	return Node{t: t, raw: *(*[4]uint64)(unsafe.Pointer(&n))}
}

func cn(n Node) C.TSNode { return *(*C.TSNode)(unsafe.Pointer(&n.raw)) }

func (t *ctsTree) nodeType(n Node) string  { return t.lang.symbol(C.ts_node_symbol(cn(n))) }
func (t *ctsTree) isNamed(n Node) bool     { return bool(C.ts_node_is_named(cn(n))) }
func (t *ctsTree) isError(n Node) bool     { return bool(C.ts_node_is_error(cn(n))) }
func (t *ctsTree) isMissing(n Node) bool   { return bool(C.ts_node_is_missing(cn(n))) }
func (t *ctsTree) hasError(n Node) bool    { return bool(C.ts_node_has_error(cn(n))) }
func (t *ctsTree) startByte(n Node) uint32 { return uint32(C.ts_node_start_byte(cn(n))) }
func (t *ctsTree) endByte(n Node) uint32   { return uint32(C.ts_node_end_byte(cn(n))) }
func (t *ctsTree) childCount(n Node) int   { return int(C.ts_node_child_count(cn(n))) }
func (t *ctsTree) parent(n Node) Node      { return t.node(C.ts_node_parent(cn(n))) }
func (t *ctsTree) id(n Node) uintptr       { return uintptr(cn(n).id) }

func (t *ctsTree) child(n Node, i int) Node {
	if i >= t.childCount(n) {
		return Node{}
	}
	return t.node(C.ts_node_child(cn(n), C.uint32_t(i)))
}

func (t *ctsTree) startPoint(n Node) Point {
	p := C.ts_node_start_point(cn(n))
	return Point{uint32(p.row), uint32(p.column)}
}

func (t *ctsTree) endPoint(n Node) Point {
	p := C.ts_node_end_point(cn(n))
	return Point{uint32(p.row), uint32(p.column)}
}

func (t *ctsTree) children(n Node, dst []Node, namedOnly bool) []Node {
	k := int(C.jt_children(t.cursor, cn(n), C.bool(namedOnly), t.buf, C.uint32_t(t.bufCap)))
	if k > t.bufCap {
		C.free(unsafe.Pointer(t.buf))
		t.bufCap = max(k, 2*t.bufCap, 64)
		t.buf = (*C.TSNode)(C.malloc(C.size_t(t.bufCap) * C.size_t(unsafe.Sizeof(C.TSNode{}))))
		k = int(C.jt_children(t.cursor, cn(n), C.bool(namedOnly), t.buf, C.uint32_t(t.bufCap)))
	}
	for _, c := range unsafe.Slice(t.buf, k) {
		dst = append(dst, Node{t: t, raw: *(*[4]uint64)(unsafe.Pointer(&c))})
	}
	return dst
}

func (t *ctsTree) childByField(n Node, field string) Node {
	id, ok := t.lang.fieldID[field]
	if !ok {
		return Node{}
	}
	return t.node(C.ts_node_child_by_field_id(cn(n), id))
}

func (t *ctsTree) fieldName(n Node) string {
	parent := C.ts_node_parent(cn(n))
	if parent.id == nil {
		return ""
	}
	id := C.jt_field(t.cursor, parent, cn(n).id)
	if int(id) < len(t.lang.fields) {
		return t.lang.fields[id]
	}
	return ""
}

// highlightQuery compiles l's highlight query on first use. It goes through
// go-tree-sitter so its text predicates (#eq?, #match?, #any-of?) can be
// checked with QueryMatch.SatisfiesTextPredicate.
func (l *ctsLang) highlightQuery() (*sitter.Query, error) {
	l.query.Do(func() {
		var src string
		for _, part := range ctsGrammars[l.name].highlight {
			src += *part + "\n"
		}
		q, qerr := sitter.NewQuery(l.sitter, src)
		if qerr != nil {
			l.qErr = fmt.Errorf("parse: C tree-sitter's %s highlight query: %v", l.name, qerr)
			return
		}
		l.q, l.qCaptures = q, q.CaptureNames()
	})
	return l.q, l.qErr
}

// cQuery is q's TSQuery, the first field of go-tree-sitter's Query.
func cQuery(q *sitter.Query) *C.TSQuery { return *(**C.TSQuery)(unsafe.Pointer(q)) }

func (t *ctsTree) highlights(tree *Tree, dst []Capture) ([]Capture, error) {
	q, err := t.lang.highlightQuery()
	if q == nil {
		return dst, err
	}
	cur := C.ts_query_cursor_new()
	defer C.ts_query_cursor_delete(cur)
	C.ts_query_cursor_exec(cur, cQuery(q), cn(tree.root))

	first := len(dst)
	var m C.TSQueryMatch
	var index C.uint32_t
	var caps []sitter.QueryCapture
	var buf1, buf2 []byte
	for C.jt_next_capture(cur, &m, &index) {
		if len(q.TextPredicates[m.pattern_index]) > 0 {
			caps = caps[:0]
			for _, c := range unsafe.Slice(m.captures, m.capture_count) {
				caps = append(caps, sitter.QueryCapture{Node: *(*sitter.Node)(unsafe.Pointer(&c.node)), Index: uint32(c.index)})
			}
			qm := sitter.QueryMatch{Captures: caps, PatternIndex: uint(m.pattern_index)}
			if !qm.SatisfiesTextPredicate(q, buf1, buf2, tree.src) {
				C.ts_query_cursor_remove_match(cur, m.id)
				continue
			}
		}
		c := unsafe.Slice(m.captures, m.capture_count)[index]
		dst = append(dst, Capture{Name: t.lang.qCaptures[c.index], Node: t.node(c.node)})
	}
	sortCaptures(dst[first:])
	return dst, nil
}

func (t *ctsTree) release() {
	C.jt_cursor_delete(t.cursor)
	C.free(unsafe.Pointer(t.buf))
	C.ts_tree_delete(t.t)
}
