package zerolog

import (
	"reflect"
)

// SafeExtractor extracts a set of safe structured fields from an error.
//
// A SafeExtractor is registered for a concrete error type (identified by its
// reflect.Type string, e.g. "*fmt.wrapError") and is called by the safe error
// chain encoder instead of the error's Error() or MarshalJSON methods. The
// returned fields are merged into the error node.
//
// The extractor must not call Error() or MarshalJSON on the error in a way
// that exposes sensitive text; it is the caller's responsibility to register
// only extractors that return safe, non-sensitive data.
type SafeExtractor func(err error) map[string]interface{}

// SafeErrorConfig controls the behavior of the safe error chain encoder.
//
// The encoder deliberately does not call Error() or MarshalJSON on arbitrary
// errors, as those methods may leak sensitive text. Instead it only emits:
//
//   - the concrete type of each error (always),
//   - the depth level of each node,
//   - the verbatim Error() text of errors whose type is registered as safe,
//   - fields returned by registered SafeExtractor functions,
//   - the cause edges (single Unwrap() and multi Unwrap() []error, e.g.
//     errors.Join),
//   - explicit markers when the chain is truncated by depth or by a cycle.
//
// The zero value is not usable; use NewSafeErrorConfig to construct one.
type SafeErrorConfig struct {
	// MaxDepth is the maximum depth of the cause chain that is expanded.
	// Nodes at depth >= MaxDepth are emitted with a "truncated":"depth"
	// marker and are not recursed into, so the output never pretends to be a
	// complete chain. Must be >= 1.
	MaxDepth int

	safeTypes      map[string]struct{}
	safeExtractors map[string]SafeExtractor
}

// DefaultSafeErrorConfig is the configuration used by Event.SafeErr and
// Context.SafeErr when no explicit configuration is supplied. It allows a
// chain of up to 32 levels and emits no verbatim error text unless a type is
// explicitly registered as safe.
var DefaultSafeErrorConfig = NewSafeErrorConfig(32)

// NewSafeErrorConfig creates a new safe error chain configuration with the
// given maximum depth. If maxDepth < 1 it is clamped to 1.
func NewSafeErrorConfig(maxDepth int) *SafeErrorConfig {
	if maxDepth < 1 {
		maxDepth = 1
	}
	return &SafeErrorConfig{
		MaxDepth:       maxDepth,
		safeTypes:      make(map[string]struct{}),
		safeExtractors: make(map[string]SafeExtractor),
	}
}

// RegisterSafeType marks errors whose concrete type name is typeName as safe
// to emit verbatim. The type name is the reflect.Type string (for example
// "*fmt.wrapError").
func (c *SafeErrorConfig) RegisterSafeType(typeName string) *SafeErrorConfig {
	if c.safeTypes == nil {
		c.safeTypes = make(map[string]struct{})
	}
	c.safeTypes[typeName] = struct{}{}
	return c
}

// RegisterSafeTypeOf marks errors with the same concrete type as err as safe
// to emit verbatim. If err is nil this is a no-op.
func (c *SafeErrorConfig) RegisterSafeTypeOf(err error) *SafeErrorConfig {
	if err != nil {
		c.RegisterSafeType(reflect.TypeOf(err).String())
	}
	return c
}

// RegisterExtractor registers a SafeExtractor for errors whose concrete type
// name is typeName. The extractor is called instead of Error()/MarshalJSON.
func (c *SafeErrorConfig) RegisterExtractor(typeName string, ext SafeExtractor) *SafeErrorConfig {
	if c.safeExtractors == nil {
		c.safeExtractors = make(map[string]SafeExtractor)
	}
	c.safeExtractors[typeName] = ext
	return c
}

// SafeErrorChain is a LogObjectMarshaler that safely encodes an error chain.
//
// It is safe to use with errors whose Error() or MarshalJSON methods have
// side effects or leak sensitive text: those methods are never called unless
// the error's concrete type is explicitly registered as safe.
type SafeErrorChain struct {
	err error
	cfg *SafeErrorConfig
}

// NewSafeErrorChain creates a SafeErrorChain marshaler for err using cfg.
// If cfg is nil, DefaultSafeErrorConfig is used.
func NewSafeErrorChain(err error, cfg *SafeErrorConfig) SafeErrorChain {
	if cfg == nil {
		cfg = DefaultSafeErrorConfig
	}
	return SafeErrorChain{err: err, cfg: cfg}
}

// MarshalZerologObject implements LogObjectMarshaler.
func (c SafeErrorChain) MarshalZerologObject(e *Event) {
	if c.err == nil || e == nil {
		return
	}
	if c.cfg == nil {
		c.cfg = DefaultSafeErrorConfig
	}

	// Pre-pass: assign a stable id to each unique error identity and record
	// which identities appear more than once (so references can be emitted).
	firstSeen := make(map[uintptr]int)
	referenced := make(map[uintptr]bool)
	nextID := 0
	collectIdentities(c.err, firstSeen, referenced, &nextID)

	// Emit pass: walk the chain, emitting references for repeated identities.
	emitted := make(map[uintptr]int)
	marshalNode(e, c.cfg, c.err, 0, firstSeen, referenced, emitted)
}

// marshalNode emits a single error node and recurses into its causes.
func marshalNode(e *Event, cfg *SafeErrorConfig, err error, depth int, firstSeen map[uintptr]int, referenced map[uintptr]bool, emitted map[uintptr]int) {
	if err == nil {
		return
	}

	typeName := reflect.TypeOf(err).String()

	// Identity-based cycle / reuse detection.
	if id, ok := errorIdentity(err); ok {
		if prev, seen := emitted[id]; seen {
			// Reference to an already-emitted error: do not re-expand.
			e.Str("type", typeName)
			e.Int("level", depth)
			e.Bool("cycle", true)
			e.Int("ref", prev)
			return
		}
		emitted[id] = firstSeen[id]
		// Emit the id only if this error is referenced elsewhere.
		if referenced[id] {
			e.Int("id", firstSeen[id])
		}
	}

	e.Str("type", typeName)
	e.Int("level", depth)

	// Depth limit: mark the node as truncated and do not recurse, so the
	// output never pretends to be a complete cause chain.
	if depth >= cfg.MaxDepth {
		e.Str("truncated", "depth")
		return
	}

	// Safe verbatim message: only for explicitly registered types.
	if _, ok := cfg.safeTypes[typeName]; ok {
		e.Str("message", err.Error())
	}

	// Safe extractor: user-provided, assumed safe; never calls Error() itself.
	if ext, ok := cfg.safeExtractors[typeName]; ok {
		for k, v := range ext(err) {
			e.Interface(k, v)
		}
	}

	// Cause edges: preserve multi-unwrap branches (errors.Join) as well as
	// single unwrap chains.
	causes := unwrapCauses(err)
	if len(causes) > 0 {
		arr := e.CreateArray()
		for _, cause := range causes {
			arr = arr.Object(safeErrorNode{
				err:        cause,
				cfg:        cfg,
				depth:      depth + 1,
				firstSeen:  firstSeen,
				referenced: referenced,
				emitted:    emitted,
			})
		}
		e.Array("causes", arr)
	}
}

// safeErrorNode adapts a recursive marshalNode call to the LogObjectMarshaler
// interface so it can be appended to an array of causes.
type safeErrorNode struct {
	err        error
	cfg        *SafeErrorConfig
	depth      int
	firstSeen  map[uintptr]int
	referenced map[uintptr]bool
	emitted    map[uintptr]int
}

func (n safeErrorNode) MarshalZerologObject(e *Event) {
	marshalNode(e, n.cfg, n.err, n.depth, n.firstSeen, n.referenced, n.emitted)
}

// collectIdentities walks the chain assigning each unique error identity a
// stable id and recording identities that appear more than once.
func collectIdentities(err error, firstSeen map[uintptr]int, referenced map[uintptr]bool, nextID *int) {
	if err == nil {
		return
	}
	id, ok := errorIdentity(err)
	if !ok {
		return
	}
	if _, seen := firstSeen[id]; seen {
		referenced[id] = true
		return
	}
	firstSeen[id] = *nextID
	*nextID++
	for _, c := range unwrapCauses(err) {
		collectIdentities(c, firstSeen, referenced, nextID)
	}
}

// errorIdentity returns a stable identity for an error for cycle detection.
// For pointer kinds it returns the pointer value; the second result reports
// whether a usable identity was found.
func errorIdentity(err error) (uintptr, bool) {
	rv := reflect.ValueOf(err)
	if rv.Kind() == reflect.Ptr && !rv.IsNil() {
		return rv.Pointer(), true
	}
	return 0, false
}

// unwrapCauses returns the cause edges of err. It preserves multi-unwrap
// branches (errors.Join) as well as single unwrap chains. Nil causes are
// filtered out.
func unwrapCauses(err error) []error {
	// Prefer multi-unwrap so errors.Join branches are all preserved.
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		branches := u.Unwrap()
		out := make([]error, 0, len(branches))
		for _, c := range branches {
			if c != nil {
				out = append(out, c)
			}
		}
		return out
	}
	// Fall back to single unwrap.
	if u, ok := err.(interface{ Unwrap() error }); ok {
		if c := u.Unwrap(); c != nil {
			return []error{c}
		}
	}
	return nil
}
