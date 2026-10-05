package zerolog

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type recordingWriter struct {
	buf bytes.Buffer
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

func (w *recordingWriter) WriteLevel(_ Level, p []byte) (int, error) {
	return w.buf.Write(p)
}

func (w *recordingWriter) String() string {
	return w.buf.String()
}

func (w *recordingWriter) Len() int {
	return w.buf.Len()
}

type countingGenerator struct {
	calls int
	val   interface{}
	err   error
}

func (g *countingGenerator) gen() func() (interface{}, error) {
	return func() (interface{}, error) {
		g.calls++
		return g.val, g.err
	}
}

// TestDeferredThreeFieldsSuccess is the three-field success demo.
// All three generators succeed and the writer sees one complete JSON record.
func TestDeferredThreeFieldsSuccess(t *testing.T) {
	w := &recordingWriter{}
	logger := New(w).Level(InfoLevel)

	g1 := &countingGenerator{val: "alpha"}
	g2 := &countingGenerator{val: 42}
	g3 := &countingGenerator{val: true}

	logger.Info().Deferred().
		Add("field1", g1.gen()).
		Add("field2", g2.gen()).
		Add("field3", g3.gen()).
		Msg("three fields")

	if g1.calls != 1 || g2.calls != 1 || g3.calls != 1 {
		t.Fatalf("generators must each run once: got %d %d %d", g1.calls, g2.calls, g3.calls)
	}

	out := w.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("record must end with a line break: %q", out)
	}
	if !strings.Contains(out, "\"field1\":\"alpha\"") {
		t.Fatalf("missing field1 in complete JSON: %q", out)
	}
	if !strings.Contains(out, "\"field2\":42") {
		t.Fatalf("missing field2 in complete JSON: %q", out)
	}
	if !strings.Contains(out, "\"field3\":true") {
		t.Fatalf("missing field3 in complete JSON: %q", out)
	}
	if !strings.Contains(out, "\"message\":\"three fields\"") {
		t.Fatalf("missing message in complete JSON: %q", out)
	}
}

// TestDeferredSecondFieldFailsReject is the acceptance test: when the second
// field fails, the writer sees no half record and the first field's generator
// ran but its value is discarded.
func TestDeferredSecondFieldFailsReject(t *testing.T) {
	w := &recordingWriter{}
	logger := New(w).Level(InfoLevel)

	g1 := &countingGenerator{val: "alpha"}
	g2 := &countingGenerator{err: errors.New("boom")}
	g3 := &countingGenerator{val: "gamma"}

	logger.Info().Deferred().
		Add("field1", g1.gen()).
		Add("field2", g2.gen()).
		Add("field3", g3.gen()).
		Msg("should not appear")

	if g1.calls != 1 {
		t.Fatalf("field1 generator must run once: got %d", g1.calls)
	}
	if g2.calls != 1 {
		t.Fatalf("field2 generator must run once: got %d", g2.calls)
	}
	if g3.calls != 0 {
		t.Fatalf("field3 generator must not run after field2 failure: got %d", g3.calls)
	}

	if w.Len() != 0 {
		t.Fatalf("writer saw a record on failure (no half record allowed): %q", w.String())
	}
}

// TestDeferredDisabledLevel is the acceptance test: when the level is
// disabled, all three generators run zero times and nothing is written.
func TestDeferredDisabledLevel(t *testing.T) {
	w := &recordingWriter{}
	logger := New(w).Level(ErrorLevel)

	g1 := &countingGenerator{val: "alpha"}
	g2 := &countingGenerator{val: "beta"}
	g3 := &countingGenerator{val: "gamma"}

	logger.Info().Deferred().
		Add("field1", g1.gen()).
		Add("field2", g2.gen()).
		Add("field3", g3.gen()).
		Msg("should not appear")

	if g1.calls != 0 || g2.calls != 0 || g3.calls != 0 {
		t.Fatalf("all generators must run 0 times when level disabled: got %d %d %d",
			g1.calls, g2.calls, g3.calls)
	}
	if w.Len() != 0 {
		t.Fatalf("writer saw a record when level disabled: %q", w.String())
	}
}

// dropSampler always drops the event.
type dropSampler struct{}

func (dropSampler) Sample(_ Level) bool { return false }

// TestDeferredSampledOut verifies that when the event is sampled out,
// generators run zero times.
func TestDeferredSampledOut(t *testing.T) {
	w := &recordingWriter{}
	logger := New(w).Level(InfoLevel).Sample(dropSampler{})

	g1 := &countingGenerator{val: "alpha"}
	g2 := &countingGenerator{val: "beta"}
	g3 := &countingGenerator{val: "gamma"}

	logger.Info().Deferred().
		Add("field1", g1.gen()).
		Add("field2", g2.gen()).
		Add("field3", g3.gen()).
		Msg("should not appear")

	if g1.calls != 0 || g2.calls != 0 || g3.calls != 0 {
		t.Fatalf("all generators must run 0 times when sampled out: got %d %d %d",
			g1.calls, g2.calls, g3.calls)
	}
	if w.Len() != 0 {
		t.Fatalf("writer saw a record when sampled out: %q", w.String())
	}
}

// TestDeferredMarkerOnFailure verifies that in MarkerOnFailure mode, a fixed
// failure marker is written and the marker never contains the error text.
func TestDeferredMarkerOnFailure(t *testing.T) {
	w := &recordingWriter{}
	logger := New(w).Level(InfoLevel)

	secretErr := errors.New("secret internal error text")
	g1 := &countingGenerator{val: "alpha"}
	g2 := &countingGenerator{err: secretErr}
	g3 := &countingGenerator{val: "gamma"}

	logger.Info().Deferred().
		OnFailure(MarkerOnFailure).
		Add("field1", g1.gen()).
		Add("field2", g2.gen()).
		Add("field3", g3.gen()).
		Msg("marker test")

	out := w.String()
	if strings.Contains(out, "secret internal error text") {
		t.Fatalf("failure marker must not contain the error text: %q", out)
	}
	if !strings.Contains(out, "\"deferred_failed\":true") {
		t.Fatalf("fixed failure marker missing: %q", out)
	}
	if !strings.Contains(out, "\"message\":\"marker test\"") {
		t.Fatalf("message missing in marker output: %q", out)
	}
	if g3.calls != 0 {
		t.Fatalf("field3 generator must not run after field2 failure: got %d", g3.calls)
	}
}

// TestDeferredGeneratorsRunAtMostOnce verifies that calling Msg twice does
// not re-run generators.
func TestDeferredGeneratorsRunAtMostOnce(t *testing.T) {
	w := &recordingWriter{}
	logger := New(w).Level(InfoLevel)

	g1 := &countingGenerator{val: "alpha"}

	d := logger.Info().Deferred().Add("field1", g1.gen())
	d.Msg("first")
	d.Msg("second")

	if g1.calls != 1 {
		t.Fatalf("generator must run at most once: got %d", g1.calls)
	}
}

// TestDeferredNoFields verifies that an event with no deferred fields is
// written normally.
func TestDeferredNoFields(t *testing.T) {
	w := &recordingWriter{}
	logger := New(w).Level(InfoLevel)

	logger.Info().Deferred().Msg("no fields")

	out := w.String()
	if !strings.Contains(out, "\"message\":\"no fields\"") {
		t.Fatalf("message missing: %q", out)
	}
}

// TestDeferredOrderIsRegistrationOrder verifies that fields appear in the
// order they were registered, not in map order or any other order.
func TestDeferredOrderIsRegistrationOrder(t *testing.T) {
	w := &recordingWriter{}
	logger := New(w).Level(InfoLevel)

	logger.Info().Deferred().
		Add("z", func() (interface{}, error) { return 1, nil }).
		Add("a", func() (interface{}, error) { return 2, nil }).
		Add("m", func() (interface{}, error) { return 3, nil }).
		Msg("order")

	out := w.String()
	zIdx := strings.Index(out, "\"z\":1")
	aIdx := strings.Index(out, "\"a\":2")
	mIdx := strings.Index(out, "\"m\":3")
	if zIdx < 0 || aIdx < 0 || mIdx < 0 {
		t.Fatalf("not all fields present: %q", out)
	}
	if !(zIdx < aIdx && aIdx < mIdx) {
		t.Fatalf("fields must appear in registration order (z, a, m): %q", out)
	}
}

// TestDeferredNilEvent verifies that Deferred on a nil event (e.g. from a
// disabled logger) does not panic and runs no generators.
func TestDeferredNilEvent(t *testing.T) {
	w := &recordingWriter{}
	logger := New(w).Level(Disabled)

	g1 := &countingGenerator{val: "alpha"}

	logger.Info().Deferred().
		Add("field1", g1.gen()).
		Msg("should not appear")

	if g1.calls != 0 {
		t.Fatalf("generator must not run on nil event: got %d", g1.calls)
	}
	if w.Len() != 0 {
		t.Fatalf("writer saw a record on nil event: %q", w.String())
	}
}

// ExampleDeferredEvent demonstrates the three-field deferred registration
// layer: generators run in registration order only when the event is enabled,
// and the event is committed only when all fields succeed.
func ExampleDeferredEvent() {
	w := &recordingWriter{}
	logger := New(w).Level(InfoLevel)

	logger.Info().Deferred().
		Add("field1", func() (interface{}, error) { return "alpha", nil }).
		Add("field2", func() (interface{}, error) { return "beta", nil }).
		Add("field3", func() (interface{}, error) { return "gamma", nil }).
		Msg("three fields")

	_ = w.String()
}
