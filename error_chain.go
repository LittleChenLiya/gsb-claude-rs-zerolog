// Package zerolog safe error-chain encoder.
//
// This file provides an explicit, side-effect-free encoder for error chains.
// Unlike the default error encoding (which is unchanged and still calls
// Error to obtain a message), the safe encoder never invokes the Error method
// of an error and never invokes MarshalJSON or MarshalZerologObject on the
// underlying error. It only reports structural information that has been
// explicitly registered as safe:
//
//   - category: a human label registered for the concrete error type,
//   - layer:    the traversal depth of the node (root is 0),
//   - cause:    parent to child edges describing Unwrap relationships,
//   - fields:   values returned by a declared safe extractor.
//
// Errors whose type has not been registered are reported by their Go type
// name only (category "unknown"); their potentially sensitive text is never
// read. The traversal is protected against cycles (an error reached a second
// time is emitted as an identity reference instead of being expanded) and
// against runaway depth (a hard, explicitly marked truncation limit).
//
// Run the focused tests with:
//
//	go test -run TestSafeErrorChain -count=1 .
//
// See ExampleErrorChain for a three-node log output example.
package zerolog

import (
	"reflect"
	"sync"
)

// ErrorChainMaxDepth is the hard limit on how many Unwrap layers are expanded
// from the root. Chains deeper than this are explicitly marked as truncated
// instead of being silently cut. It exists independently of cycle detection so
// that non-repeating but very deep chains are also bounded.
const ErrorChainMaxDepth = 32

// SafeErrorExtractor returns safe, non-sensitive structured fields for an
// error. It MUST NOT call Error or MarshalJSON, and MUST NOT otherwise read
// free-form error text. Only an extractor explicitly registered for an error
// type through RegisterSafeErrorType is ever invoked by the safe encoder.
type SafeErrorExtractor func(err error) map[string]interface{}

type errorTypeInfo struct {
	category  string
	extractor SafeErrorExtractor
}

var (
	errorTypeRegistryMu sync.RWMutex
	// errorTypeRegistry maps a concrete error type to its safe metadata. An
	// absent entry means the object is unknown: report its type name only.
	errorTypeRegistry = map[reflect.Type]errorTypeInfo{}
)

// RegisterSafeErrorType declares the safe category (and, optionally, a safe
// field extractor) for errors sharing the concrete type of example. Only
// registered types carry a category or extracted fields; the extractor is
// never required. A nil example is ignored. Registering a type again replaces
// the previous declaration.
func RegisterSafeErrorType(example error, category string, extractor SafeErrorExtractor) {
	if example == nil {
		return
	}
	registerType(reflect.TypeOf(example), category, extractor)
}

// RegisterSafeErrorTypeT is the reflect.Type based variant of
// RegisterSafeErrorType, useful when constructing an example is awkward.
func RegisterSafeErrorTypeT(typ reflect.Type, category string, extractor SafeErrorExtractor) {
	if typ == nil {
		return
	}
	registerType(typ, category, extractor)
}

func registerType(typ reflect.Type, category string, extractor SafeErrorExtractor) {
	errorTypeRegistryMu.Lock()
	errorTypeRegistry[typ] = errorTypeInfo{category: category, extractor: extractor}
	errorTypeRegistryMu.Unlock()
}

func lookupTypeInfo(typ reflect.Type) errorTypeInfo {
	errorTypeRegistryMu.RLock()
	info := errorTypeRegistry[typ]
	errorTypeRegistryMu.RUnlock()
	return info
}

// ErrorChainOption configures a safe error-chain encoding.
type ErrorChainOption func(*chainConfig)

type chainConfig struct {
	maxDepth        int
	useExtractors   bool
	categoryField   string
	layerField      string
	typeField       string
	unknownCategory string
}

func defaultChainConfig() chainConfig {
	return chainConfig{
		maxDepth:        ErrorChainMaxDepth,
		useExtractors:   true,
		categoryField:   "category",
		layerField:      "layer",
		typeField:       "type",
		unknownCategory: "unknown",
	}
}

// ErrorChainWithMaxDepth overrides the number of expanded Unwrap layers.
// Values below 1 are clamped to 1 so the root is always emitted.
func ErrorChainWithMaxDepth(depth int) ErrorChainOption {
	return func(c *chainConfig) {
		if depth < 1 {
			depth = 1
		}
		c.maxDepth = depth
	}
}

// ErrorChainWithoutExtractors disables calling any registered safe extractor.
// Structure, categories and references are still reported.
func ErrorChainWithoutExtractors() ErrorChainOption {
	return func(c *chainConfig) { c.useExtractors = false }
}

// chainNode describes one unique error identity in the graph.
type chainNode struct {
	id        int
	layer     int
	typeName  string
	category  string
	known     bool
	fields    map[string]interface{}
	truncated bool // expansion stopped because the depth limit was reached
}

// chainEdge is a directed cause relationship from a parent (from) to the error
// it wraps (to). A multi-cause error such as errors.Join contributes one edge
// per branch, so every branch is preserved.
type chainEdge struct {
	from int
	to   int
}

// tokenIdentity pairs an identity token with the node it refers to.
type tokenIdentity struct {
	token interface{}
	id    int
}

// ErrorChain is the safe, side-effect-free encoding of an error chain. It
// implements LogObjectMarshaler so it can be attached with Event.Object without
// ever invoking Error or MarshalJSON on the underlying errors.
type ErrorChain struct {
	root      int
	nodes     []chainNode
	edges     []chainEdge
	tokens    []tokenIdentity
	cfg       chainConfig
	hasRoot   bool
	truncated bool // a node was cut by the depth limit
}

// NewErrorChain walks err using structural unwrapping only and returns its safe
// graph representation. It never calls Error or any MarshalJSON method. A nil
// error yields a chain with no root (MarshalZerologObject emits null).
func NewErrorChain(err error, opts ...ErrorChainOption) *ErrorChain {
	cfg := defaultChainConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	chain := &ErrorChain{
		root: -1,
		cfg:  cfg,
	}
	if err != nil {
		chain.hasRoot = true
		chain.root = chain.visit(err, nil, 0)
	}
	return chain
}

// ErrChain attaches the safe chain of err under the given key. It is a
// convenience wrapper around NewErrorChain. A nil error writes null.
func (e *Event) ErrChain(key string, err error, opts ...ErrorChainOption) *Event {
	if e == nil {
		return e
	}
	if err == nil {
		e.buf = enc.AppendNil(enc.AppendKey(e.buf, key))
		return e
	}
	return e.Object(key, NewErrorChain(err, opts...))
}

// identity returns a stable token for the identity of an error. Pointer
// bearing errors are keyed by their address so the same error reused across
// branches resolves to a single node. Non-comparable values yield a nil token
// (not tracked across references); they remain bounded by the depth limit and
// can never alias unrelated values.
func identity(err error) (token interface{}, tracked bool) {
	v := reflect.ValueOf(err)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		if v.IsNil() {
			return nil, false
		}
		return [2]interface{}{v.Type(), v.Pointer()}, true
	case reflect.Interface:
		if v.IsNil() {
			return nil, false
		}
		if inner, ok := v.Elem().Interface().(error); ok {
			return identity(inner)
		}
		return nil, false
	default:
		if v.Type().Comparable() {
			return [2]interface{}{v.Type(), v.Interface()}, true
		}
		return nil, false
	}
}

// visit inserts err into the graph (when not already present) and returns its
// node id. parent is the enclosing error (nil for the root). A repeat becomes
// an identity reference, and depth is capped with an explicit marker.
func (c *ErrorChain) visit(err error, parent error, layer int) int {
	typ := reflect.TypeOf(err)
	token, tracked := identity(err)

	if tracked {
		if id, ok := c.findNode(token); ok {
			if parent != nil {
				c.addEdge(parent, id)
			}
			return id
		}
	}

	info := lookupTypeInfo(typ)
	node := chainNode{
		id:       len(c.nodes),
		layer:    layer,
		typeName: typeName(typ),
		known:    info.category != "",
		category: info.category,
	}
	if node.category == "" {
		node.category = c.cfg.unknownCategory
	}
	if c.cfg.useExtractors && info.extractor != nil {
		// The extractor is user supplied and explicitly declared safe; the
		// encoder itself still never reads error text.
		node.fields = info.extractor(err)
	}

	c.nodes = append(c.nodes, node)
	id := node.id
	if tracked {
		c.tokens = append(c.tokens, tokenIdentity{token: token, id: id})
	}
	if parent != nil {
		c.addEdge(parent, id)
	}

	// A layer at or beyond the limit is marked truncated and is not expanded.
	if layer >= c.cfg.maxDepth {
		c.nodes[id].truncated = true
		c.truncated = true
		return id
	}

	for _, cause := range causesOf(err) {
		c.visit(cause, err, layer+1)
	}
	return id
}

// causesOf returns the structural causes of err without ever reading its text.
// It uses only the single and multi-cause Unwrap interfaces.
func causesOf(err error) []error {
	if err == nil {
		return nil
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		return nonNilErrors(u.Unwrap())
	}
	if u, ok := err.(errUnwrapper); ok {
		if cause := u.Unwrap(); cause != nil {
			return []error{cause}
		}
	}
	return nil
}

// errUnwrapper is the standard single-cause unwrap interface.
type errUnwrapper interface {
	Unwrap() error
}

func nonNilErrors(errs []error) []error {
	out := make([]error, 0, len(errs))
	for _, e := range errs {
		if e != nil && !isNilValue(e) {
			out = append(out, e)
		}
	}
	return out
}

func (c *ErrorChain) findNode(token interface{}) (int, bool) {
	for _, t := range c.tokens {
		if t.token == token {
			return t.id, true
		}
	}
	return 0, false
}

// addEdge records a parent error to child node cause edge. The parent must
// already have a node. Repeated edges for the same pair are de-duplicated.
func (c *ErrorChain) addEdge(parent error, to int) {
	token, tracked := identity(parent)
	if !tracked {
		return
	}
	from, ok := c.findNode(token)
	if !ok {
		return
	}
	for _, e := range c.edges {
		if e.from == from && e.to == to {
			return
		}
	}
	c.edges = append(c.edges, chainEdge{from: from, to: to})
}

func typeName(t reflect.Type) string {
	if t == nil {
		return ""
	}
	if name := t.Name(); name != "" {
		if pkg := t.PkgPath(); pkg != "" {
			return pkg + "." + name
		}
		return name
	}
	return t.String()
}

// MarshalZerologObject writes the safe graph. It is the only marshaling path
// and performs no Error or MarshalJSON calls.
func (c *ErrorChain) MarshalZerologObject(e *Event) {
	if c == nil || !c.hasRoot {
		// Reached through Object with an empty chain; emit an explicit null.
		e.buf = enc.AppendNil(e.buf)
		return
	}

	e.Int("root", c.root)

	nodesArr := e.CreateArray()
	for i := range c.nodes {
		nodesArr.Object(chainNodeMarshaler{node: &c.nodes[i], cfg: &c.cfg})
	}
	e.Array("nodes", nodesArr)

	edgesArr := e.CreateArray()
	for i := range c.edges {
		edgesArr.Object(chainEdgeMarshaler{edge: &c.edges[i]})
	}
	e.Array("edges", edgesArr)

	if c.truncated {
		e.Bool("truncated", true)
	}
}

// chainNodeMarshaler renders a single node using only registered-safe data.
type chainNodeMarshaler struct {
	node *chainNode
	cfg  *chainConfig
}

func (m chainNodeMarshaler) MarshalZerologObject(e *Event) {
	e.Int("id", m.node.id)
	e.Str(m.cfg.typeField, m.node.typeName)
	if m.node.known {
		e.Str(m.cfg.categoryField, m.node.category)
	} else {
		// Unknown objects report their type only; category stays unknown.
		e.Str(m.cfg.categoryField, m.cfg.unknownCategory)
	}
	e.Int(m.cfg.layerField, m.node.layer)
	if m.node.truncated {
		e.Bool("truncated", true)
	}
	if len(m.node.fields) > 0 {
		dict := e.CreateDict()
		for k, v := range m.node.fields {
			dict.Interface(k, v)
		}
		e.Dict("fields", dict)
	}
}

// chainEdgeMarshaler renders one parent to child cause edge.
type chainEdgeMarshaler struct {
	edge *chainEdge
}

func (m chainEdgeMarshaler) MarshalZerologObject(e *Event) {
	e.Int("from", m.edge.from)
	e.Int("to", m.edge.to)
}
