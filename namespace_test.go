//go:build !binary_log
// +build !binary_log

package zerolog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// countingWriter counts successful WriteLevel calls and bytes written, so
// tests can assert that a rejected event never reaches the underlying
// writer (no half log line).
type countingWriter struct {
	lines *[][]byte
	calls int
	bytes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	return w.WriteLevel(InfoLevel, p)
}

func (w *countingWriter) WriteLevel(_ Level, p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	*w.lines = append(*w.lines, cp)
	w.calls++
	w.bytes += len(p)
	return len(p), nil
}

// mustUnmarshalLine decodes exactly one JSON object from got and requires it
// to be the only content (apart from the trailing newline).
func mustUnmarshalLine(t testing.TB, got []byte) map[string]interface{} {
	t.Helper()
	got = bytes.TrimRight(got, "\n")
	if !bytes.HasPrefix(got, []byte{'{'}) || !bytes.HasSuffix(got, []byte{'}'}) {
		t.Fatalf("expected one JSON object, got %q", got)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("invalid JSON %q: %v", got, err)
	}
	return m
}

// TestNamespaceTwoComponentsSameID: two components both write "id" and the
// output keeps them isolated as db.id and http.id (per-component objects).
func TestNamespaceTwoComponentsSameID(t *testing.T) {
	var lines [][]byte
	w := &countingWriter{lines: &lines}
	log := New(w).Level(InfoLevel)

	log.Info().Namespace(NamespaceDupReject).
		Component("db").Str("id", "42").Int("rows", 3).
		Component("http").Str("id", "req-7").Int("status", 200).
		Msg("handled")

	if w.calls != 1 {
		t.Fatalf("writer called %d times, want 1", w.calls)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	m := mustUnmarshalLine(t, lines[0])

	db, ok := m["db"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing db object, got %v", m)
	}
	httpc, ok := m["http"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing http object, got %v", m)
	}
	if db["id"] != "42" {
		t.Errorf("db.id = %v, want 42", db["id"])
	}
	if httpc["id"] != "req-7" {
		t.Errorf("http.id = %v, want req-7", httpc["id"])
	}
	if db["rows"].(float64) != 3 || httpc["status"].(float64) != 200 {
		t.Errorf("unexpected scalar fields: %#v / %#v", db, httpc)
	}
	// Reserved fields written by the core stay at the top level.
	if m["level"] != "info" || m["message"] != "handled" {
		t.Errorf("core fields missing or moved: %v", m)
	}
	if _, exists := db["level"]; exists {
		t.Errorf("reserved level leaked into db namespace")
	}
}

// TestNamespaceReservedFieldRejected: a component attempting to write a
// reserved field (time/level/message/error/...) is rejected, validation
// happens at submit time and the underlying writer receives nothing.
func TestNamespaceReservedFieldRejected(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	log := New(w).Level(InfoLevel)

	var reported []error
	prevHandler := ErrorHandler
	ErrorHandler = func(err error) { reported = append(reported, err) }
	defer func() { ErrorHandler = prevHandler }()

	log.Info().Namespace(NamespaceDupReject).
		Component("db").Str("id", "1").
		Str("level", "sneaky"). // reserved: rejected
		Msg("x")

	if len(got) != 0 {
		t.Fatalf("writer must not receive a rejected (half) event, got %q", got)
	}
	if len(reported) != 1 {
		t.Fatalf("expected 1 reported validation error, got %d", len(reported))
	}
	var ne *NamespaceError
	if !errors.As(reported[0], &ne) || ne.Key != "level" {
		t.Fatalf("expected NamespaceError for level, got %v", reported[0])
	}
}

// TestNamespaceDuplicateRejectPolicy: in the same component, writing "id"
// twice with NamespaceDupReject collects an error and the whole event is
// refused at submit time.
func TestNamespaceDuplicateRejectPolicy(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	log := New(w).Level(InfoLevel)

	n := log.Info().Namespace(NamespaceDupReject).
		Component("db").
		Str("id", "first").
		Str("id", "second")

	if n.Err() == nil {
		t.Fatalf("expected Err() to surface the duplicate before submit")
	}
	n.Msg("dup")

	if len(got) != 0 {
		t.Fatalf("rejected duplicate must not be written, got %q", got)
	}
}

// TestNamespaceDuplicateKeepFirstPolicy: in the same component, writing "id"
// twice with NamespaceDupKeepFirst keeps only the first value and the event
// is emitted normally.
func TestNamespaceDuplicateKeepFirstPolicy(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	log := New(w).Level(InfoLevel)

	n := log.Info().Namespace(NamespaceDupKeepFirst).
		Component("db").
		Str("id", "first").
		Str("id", "second")

	if err := n.Err(); err != nil {
		t.Fatalf("keep-first policy must not report errors, got %v", err)
	}
	n.Msg("keep")

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d", len(got))
	}
	m := mustUnmarshalLine(t, got[0])
	db := m["db"].(map[string]interface{})
	if db["id"] != "first" {
		t.Fatalf("db.id = %v, want first (first value retained)", db["id"])
	}
}

// TestNamespaceDuplicatesAcrossComponentsDoNotCollide: the same key in two
// different components is not a duplicate; both values survive.
func TestNamespaceDuplicatesAcrossComponentsDoNotCollide(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	log := New(w).Level(InfoLevel)

	log.Info().Namespace(NamespaceDupReject).
		Component("db").Str("id", "a").
		Component("http").Str("id", "b").
		Msg("ok")

	if len(got) != 1 {
		t.Fatalf("expected 1 line, got %d", len(got))
	}
	m := mustUnmarshalLine(t, got[0])
	if m["db"].(map[string]interface{})["id"] != "a" ||
		m["http"].(map[string]interface{})["id"] != "b" {
		t.Fatalf("cross-component ids collided: %v", m)
	}
}

// TestNamespaceNestedObjectKeepsOwnNamespace: a nested object inside a
// component is emitted under the component and its inner keys do not
// collide with the component's siblings.
func TestNamespaceNestedObjectKeepsOwnNamespace(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	log := New(w).Level(InfoLevel)

	pool := &connPool{primary: "db-primary", replica: "db-replica"}

	log.Info().Namespace(NamespaceDupReject).
		Component("db").Str("id", "7").
		Object("pool", pool).
		Msg("nested")

	if len(got) != 1 {
		t.Fatalf("expected 1 line, got %d", len(got))
	}
	m := mustUnmarshalLine(t, got[0])
	db := m["db"].(map[string]interface{})
	if db["id"] != "7" {
		t.Fatalf("db.id missing/wrong: %v", db)
	}
	p, ok := db["pool"].(map[string]interface{})
	if !ok {
		t.Fatalf("nested db.pool object missing: %v", db)
	}
	if p["primary"] != "db-primary" || p["replica"] != "db-replica" {
		t.Fatalf("nested object fields wrong: %v", p)
	}
}

// TestNamespaceDisabledEventSkipsExpensiveFields: when the level disables
// the event (or sampling drops it), no field construction callback runs and
// the writer is never called.
func TestNamespaceDisabledEventSkipsExpensiveFields(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	log := New(w).Level(ErrorLevel) // debug is filtered out

	built := 0
	n := log.Debug().Namespace(NamespaceDupReject)
	if n.Enabled() {
		t.Fatalf("namespaced debug event must report disabled")
	}
	n.Func(func(n *NamespaceEvent) {
		built++ // must never run for a disabled event
		n.Component("db").Str("id", "expensive")
	})
	// Direct expensive-looking calls are also no-ops and must not panic.
	n.Component("db").Str("id", "x").Msg("dropped")

	if built != 0 {
		t.Fatalf("expensive field constructor ran %d times for disabled event", built)
	}
	if len(got) != 0 {
		t.Fatalf("disabled event must not be written, got %q", got)
	}
}

// TestNamespaceSamplerStillApplies: a sampler that drops an event yields a
// disabled wrapper, preserving existing sampling behavior.
func TestNamespaceSamplerStillApplies(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	// Keep every 3rd event (1st kept, 2nd dropped, 3rd kept). One shared
	// sampler drives both events.
	sampler := &BasicSampler{N: 3}
	log := New(w).Sample(sampler)

	first := log.Info().Namespace(NamespaceDupReject)
	if !first.Enabled() {
		t.Fatalf("the first of three events should be sampled in")
	}
	first.Component("db").Str("id", "kept").Msg("sampled-in")

	second := log.Info().Namespace(NamespaceDupReject)
	if second.Enabled() {
		t.Fatalf("the second event is sampled out and must be disabled")
	}
	second.Func(func(n *NamespaceEvent) {
		t.Errorf("Func callback must not run for a sampled-out event")
	}).Component("db").Str("id", "dropped").Msg("sampled-out")

	if len(got) != 1 {
		t.Fatalf("exactly one (kept) line expected, got %q", got)
	}
	m := mustUnmarshalLine(t, got[0])
	if m["db"].(map[string]interface{})["id"] != "kept" {
		t.Fatalf("wrong (or dropped) event emitted: %v", m)
	}
}

// TestNamespacePlainEventHotPathUnchanged: an event that never calls
// Namespace behaves exactly as before (flat prefixed fields, no wrapper).
func TestNamespacePlainEventHotPathUnchanged(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	log := New(w).Level(InfoLevel)

	log.Info().Str("id", "plain").Str("component", "core").Msg("hot")

	if len(got) != 1 {
		t.Fatalf("expected 1 line, got %d", len(got))
	}
	m := mustUnmarshalLine(t, got[0])
	if m["id"] != "plain" || m["component"] != "core" {
		t.Fatalf("plain event output changed: %v", m)
	}
}

// TestNamespaceTopLevelFieldsAndMultipleComponents covers fields attached
// before the first Component call plus several components in one event.
func TestNamespaceTopLevelFieldsAndMultipleComponents(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	log := New(w).Level(InfoLevel)

	log.Info().Namespace(NamespaceDupKeepFirst).
		Str("trace", "abc").
		Component("db").Str("id", "1").
		Component("cache").Str("hit", "true").Bool("warm", true).
		Msg("multi")

	m := mustUnmarshalLine(t, got[0])
	if m["trace"] != "abc" {
		t.Errorf("top-level business field missing: %v", m)
	}
	if m["db"].(map[string]interface{})["id"] != "1" {
		t.Errorf("db.id wrong: %v", m)
	}
	cache := m["cache"].(map[string]interface{})
	if cache["hit"] != "true" || cache["warm"] != true {
		t.Errorf("cache fields wrong: %v", cache)
	}
}

// captureWriter is a LevelWriter that retains every written log line, so
// tests can assert both the count (atomicity) and the content of writes.
type captureWriter struct {
	lines *[][]byte
}

func (w *captureWriter) Write(p []byte) (int, error) {
	return w.WriteLevel(InfoLevel, p)
}

func (w *captureWriter) WriteLevel(_ Level, p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	*w.lines = append(*w.lines, cp)
	return len(p), nil
}

// connPool is a LogObjectMarshaler used to verify nested objects keep their
// own namespace.
type connPool struct {
	primary string
	replica string
}

func (p *connPool) MarshalZerologObject(e *Event) {
	e.Str("primary", p.primary).Str("replica", p.replica)
}

// TestNamespaceTopLevelReservedFieldRejected: the reserved-field rule also
// applies to fields written before the first Component call.
func TestNamespaceTopLevelReservedFieldRejected(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	log := New(w).Level(InfoLevel)

	log.Info().Namespace(NamespaceDupReject).
		Str("time", "forged"). // reserved, even at top level
		Component("db").Str("id", "1").
		Msg("x")

	if len(got) != 0 {
		t.Fatalf("event with forged reserved field must be refused, got %q", got)
	}
}

// TestNamespaceFatalRejectedStillExits: a rejected Fatal event is not
// written but the fatal interceptor still runs (the process/request must
// terminate), consistent with zerolog's Fatal contract.
func TestNamespaceFatalRejectedStillExits(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	log := New(w).Level(InfoLevel)

	fatalCalled := false
	oldFatalExitFunc := FatalExitFunc
	FatalExitFunc = func() { fatalCalled = true }
	defer func() { FatalExitFunc = oldFatalExitFunc }()

	log.Fatal().Namespace(NamespaceDupReject).
		Component("db").Str("id", "1").Str("level", "x").
		Msg("boom")

	if len(got) != 0 {
		t.Fatalf("rejected fatal event must not be written, got %q", got)
	}
	if !fatalCalled {
		t.Fatalf("FatalExitFunc must still run for a rejected fatal event")
	}
}

// TestNamespaceDoubleSubmitIsOnce: submitting twice must never produce two
// lines or reuse a pooled event incorrectly.
func TestNamespaceDoubleSubmitIsOnce(t *testing.T) {
	var got [][]byte
	w := &captureWriter{lines: &got}
	log := New(w).Level(InfoLevel)

	n := log.Info().Namespace(NamespaceDupKeepFirst).
		Component("db").Str("id", "1")
	n.Msg("first")
	n.Msg("second")

	if len(got) != 1 {
		t.Fatalf("double submit must write exactly one line, got %d", len(got))
	}
}

// ExampleNamespaceEvent shows the on-the-wire JSON protocol: a short log
// line with per-component objects and the core reserved fields.
func ExampleNamespaceEvent() {
	w := &bytes.Buffer{}
	log := New(w).Level(InfoLevel)

	log.Info().Namespace(NamespaceDupReject).
		Component("db").Str("id", "42").
		Component("http").Str("id", "req-7").Int("status", 200).
		Msg("handled")

	line := w.String()
	line = strings.TrimSpace(line)
	// Re-marshal through a map to print deterministically, while still
	// showing the actual field layout produced by the logger.
	var m map[string]interface{}
	_ = json.Unmarshal([]byte(line), &m)
	out, _ := json.Marshal(m)
	fmt.Println(string(out))
	// Output: {"db":{"id":"42"},"http":{"id":"req-7","status":200},"level":"info","message":"handled"}
}
