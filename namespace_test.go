//go:build !binary_log

package zerolog

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// countingWriter counts the number of Write calls.
type countingWriter struct {
	buf   bytes.Buffer
	count int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.count++
	return w.buf.Write(p)
}

func (w *countingWriter) WriteLevel(level Level, p []byte) (int, error) {
	w.count++
	return w.buf.Write(p)
}

// testObject is a simple LogObjectMarshaler for nested object tests.
type testObject struct {
	rows int
}

func (o testObject) MarshalZerologObject(e *Event) {
	e.Int("rows", o.rows)
}

// testNSObject implements both LogObjectMarshaler and NamespacedLogObjectMarshaler
// for flattened prefixing.
type testNSObject struct {
	rows int
}

func (o testNSObject) MarshalZerologObject(e *Event) {
	e.Int("rows", o.rows)
}

func (o testNSObject) MarshalZerologNamespacedObject(e *NamespacedEvent) {
	e.Int("rows", o.rows)
}

func TestNamespace_TwoComponents(t *testing.T) {
	w := &countingWriter{}
	logger := New(w)

	db := logger.Info().Namespace("db")
	http := logger.Info().Namespace("http")

	db.Str("id", "db-1").Msg("query")
	http.Str("id", "http-1").Msg("request")

	got := w.buf.String()
	if !strings.Contains(got, `"db.id":"db-1"`) {
		t.Errorf("expected db.id field, got: %s", got)
	}
	if !strings.Contains(got, `"http.id":"http-1"`) {
		t.Errorf("expected http.id field, got: %s", got)
	}
	if w.count != 2 {
		t.Errorf("expected 2 writes, got %d", w.count)
	}
}

func TestNamespace_ReservedFieldRejected(t *testing.T) {
	w := &countingWriter{}
	logger := New(w)

	var captured error
	old := ErrorHandler
	ErrorHandler = func(err error) { captured = err }
	defer func() { ErrorHandler = old }()

	ns := logger.Info().Namespace("db")
	ns.Str("time", "2026-01-01").Msg("test")

	if captured == nil {
		t.Error("expected error for reserved field, got nil")
	}
	if w.count != 0 {
		t.Errorf("expected 0 writes on reserved field rejection, got %d", w.count)
	}
	if w.buf.Len() != 0 {
		t.Errorf("expected empty buffer on rejection, got: %s", w.buf.String())
	}
}

func TestNamespace_ReservedLevelAndMessage(t *testing.T) {
	for _, field := range []string{"level", "message"} {
		w := &countingWriter{}
		logger := New(w)
		var captured error
		old := ErrorHandler
		ErrorHandler = func(err error) { captured = err }
		ns := logger.Info().Namespace("db")
		ns.Str(field, "bad").Msg("test")
		ErrorHandler = old
		if captured == nil {
			t.Errorf("expected error for reserved field %q, got nil", field)
		}
		if w.count != 0 {
			t.Errorf("field %q: expected 0 writes, got %d", field, w.count)
		}
	}
}

func TestNamespace_DuplicateReject(t *testing.T) {
	w := &countingWriter{}
	logger := New(w)

	var captured error
	old := ErrorHandler
	ErrorHandler = func(err error) { captured = err }
	defer func() { ErrorHandler = old }()

	ns := logger.Info().Namespace("db")
	ns.Str("id", "first").Str("id", "second").Msg("test")

	if captured == nil {
		t.Error("expected duplicate error, got nil")
	}
	if w.count != 0 {
		t.Errorf("expected 0 writes on duplicate rejection, got %d", w.count)
	}
}

func TestNamespace_DuplicateKeepFirst(t *testing.T) {
	w := &countingWriter{}
	logger := New(w)

	ns := logger.Info().Namespace("db", WithDuplicateStrategy(DuplicateFieldKeepFirst))
	ns.Str("id", "first").Str("id", "second").Msg("test")

	got := w.buf.String()
	if !strings.Contains(got, `"db.id":"first"`) {
		t.Errorf("expected first value kept, got: %s", got)
	}
	if strings.Contains(got, "second") {
		t.Errorf("second value should have been dropped, got: %s", got)
	}
	if w.count != 1 {
		t.Errorf("expected 1 write, got %d", w.count)
	}
}

func TestNamespace_DisabledEventNoExpensiveConstruction(t *testing.T) {
	w := &countingWriter{}
	logger := New(w).Level(ErrorLevel) // Info is disabled

	called := false
	ns := logger.Info().Namespace("db")
	ns.Func(func(e *NamespacedEvent) {
		called = true
		e.Str("id", "expensive")
	})
	ns.Msg("test")

	if called {
		t.Error("expensive field construction should not run for disabled events")
	}
	if w.count != 0 {
		t.Errorf("expected 0 writes for disabled event, got %d", w.count)
	}
}

func TestNamespace_NestedObject(t *testing.T) {
	w := &countingWriter{}
	logger := New(w)

	ns := logger.Info().Namespace("db")
	ns.Object("stats", testObject{rows: 42}).Msg("query")

	got := w.buf.String()
	if !strings.Contains(got, `"db.stats":`) {
		t.Errorf("expected db.stats nested object, got: %s", got)
	}
	if !strings.Contains(got, `"rows":42`) {
		t.Errorf("expected rows inside nested object, got: %s", got)
	}
	if w.count != 1 {
		t.Errorf("expected 1 write, got %d", w.count)
	}
}

func TestNamespace_NamespacedObjectFlattened(t *testing.T) {
	w := &countingWriter{}
	logger := New(w)

	ns := logger.Info().Namespace("db")
	ns.Object("stats", testNSObject{rows: 42}).Msg("query")

	got := w.buf.String()
	if !strings.Contains(got, `"db.stats.rows":42`) {
		t.Errorf("expected flattened db.stats.rows, got: %s", got)
	}
	if w.count != 1 {
		t.Errorf("expected 1 write, got %d", w.count)
	}
}

func TestNamespace_Msgf(t *testing.T) {
	w := &countingWriter{}
	logger := New(w)

	ns := logger.Info().Namespace("db")
	ns.Str("id", "1").Msgf("hello %s", "world")

	got := w.buf.String()
	if !strings.Contains(got, `"message":"hello world"`) {
		t.Errorf("expected formatted message, got: %s", got)
	}
}

func TestNamespace_Send(t *testing.T) {
	w := &countingWriter{}
	logger := New(w)

	ns := logger.Info().Namespace("db")
	ns.Str("id", "1").Send()

	got := w.buf.String()
	if !strings.Contains(got, `"db.id":"1"`) {
		t.Errorf("expected db.id field, got: %s", got)
	}
}

func TestNamespace_MsgFuncNotCalledWhenDisabled(t *testing.T) {
	w := &countingWriter{}
	logger := New(w).Level(ErrorLevel)

	called := false
	ns := logger.Info().Namespace("db")
	ns.MsgFunc(func() string {
		called = true
		return "expensive message"
	})

	if called {
		t.Error("MsgFunc createMsg should not be called for disabled events")
	}
}

func TestNamespace_IntAndOtherTypes(t *testing.T) {
	w := &countingWriter{}
	logger := New(w)

	ns := logger.Info().Namespace("db")
	ns.Int("count", 5).Bool("ok", true).Float64("ratio", 0.5).Msg("test")

	got := w.buf.String()
	for _, want := range []string{`"db.count":5`, `"db.ok":true`, `"db.ratio":0.5`} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %s in output, got: %s", want, got)
		}
	}
}

func TestNamespace_ErrorField(t *testing.T) {
	w := &countingWriter{}
	logger := New(w)

	ns := logger.Info().Namespace("db")
	ns.Err(errors.New("connection failed")).Msg("test")

	got := w.buf.String()
	if !strings.Contains(got, `"db.error":"connection failed"`) {
		t.Errorf("expected db.error field, got: %s", got)
	}
}

func TestNamespace_ContextFieldsNotNamespaced(t *testing.T) {
	w := &countingWriter{}
	logger := New(w).With().Str("request_id", "abc").Logger()

	ns := logger.Info().Namespace("db")
	ns.Str("id", "1").Msg("test")

	got := w.buf.String()
	if !strings.Contains(got, `"request_id":"abc"`) {
		t.Errorf("context field should not be namespaced, got: %s", got)
	}
	if !strings.Contains(got, `"db.id":"1"`) {
		t.Errorf("component field should be namespaced, got: %s", got)
	}
}
