package zerolog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// okGen returns a generator that records its invocation and succeeds.
func okGen(calls *int, val interface{}) DeferredFieldGenerator {
	return func() (interface{}, error) {
		*calls++
		return val, nil
	}
}

func TestDeferredSuccessWritesCompleteJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf)

	var order []string
	calls := map[string]int{}
	mk := func(name string, val interface{}) DeferredFieldGenerator {
		return func() (interface{}, error) {
			calls[name]++
			order = append(order, name)
			return val, nil
		}
	}

	logger.Info().
		Defer("first", mk("first", 1)).
		Defer("second", mk("second", "two")).
		Defer("third", mk("third", true)).
		MsgDeferred("done")

	// Each generator ran exactly once, in registration order.
	for _, name := range []string{"first", "second", "third"} {
		if calls[name] != 1 {
			t.Fatalf("generator %q ran %d times, want exactly 1", name, calls[name])
		}
	}
	if got := fmt.Sprint(order); got != "[first second third]" {
		t.Fatalf("generators ran in order %s, want registration order [first second third]", got)
	}

	// The output is one complete JSON record containing every field.
	var m map[string]interface{}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("output is not a complete JSON record: %v (%q)", err, buf.String())
	}
	if m["first"] != float64(1) || m["second"] != "two" || m["third"] != true {
		t.Fatalf("missing deferred fields in %v", m)
	}
	if m["level"] != "info" || m["message"] != "done" {
		t.Fatalf("missing standard fields in %v", m)
	}
}

func TestDeferredRejectWritesNoPartialRecord(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf)

	var one, two, three int
	sentinel := errors.New("db connection refused: password=hunter2")

	logger.Info().
		Str("static", "value").
		Defer("one", okGen(&one, 1)).
		Defer("two", func() (interface{}, error) {
			two++
			return nil, sentinel
		}).
		Defer("three", okGen(&three, 3)).
		MsgDeferred("must not appear")

	// The writer must not see half a record.
	if buf.Len() != 0 {
		t.Fatalf("writer saw a partial record: %q", buf.String())
	}
	// Evaluation stopped at the failing (second) generator.
	if one != 1 || two != 1 {
		t.Fatalf("generators before/at failure must run once, got one=%d two=%d", one, two)
	}
	if three != 0 {
		t.Fatalf("generator after the failure ran %d times, want 0", three)
	}
}

func TestDeferredMarkPolicyWritesFixedMarker(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf)

	secret := errors.New("pq: password authentication failed for user \"root\"")

	logger.Info().
		DeferOnFailure(DeferMark).
		Defer("one", func() (interface{}, error) { return 1, nil }).
		Defer("two", func() (interface{}, error) { return nil, secret }).
		Defer("three", func() (interface{}, error) { return 3, nil }).
		MsgDeferred("marked")

	out := buf.String()

	// The failed field carries the fixed failure marker.
	if !strings.Contains(out, `"two":"`+DeferredFailureMarker+`"`) {
		t.Fatalf("failed field does not carry the fixed marker: %q", out)
	}
	// The marker must not contain the original error text.
	if strings.Contains(out, secret.Error()) || strings.Contains(out, "password authentication") {
		t.Fatalf("output leaks the original error text: %q", out)
	}
	// The record is still complete, valid JSON.
	var m map[string]interface{}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("output is not a complete JSON record: %v (%q)", err, out)
	}
	if m["two"] != DeferredFailureMarker || m["one"] != float64(1) || m["three"] != float64(3) {
		t.Fatalf("unexpected fields in %v", m)
	}
}

func TestDeferredDisabledLevelRunsZeroGenerators(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf).Level(WarnLevel)

	calls := 0
	gen := okGen(&calls, 1)

	logger.Debug().
		Defer("a", gen).
		Defer("b", gen).
		Defer("c", gen).
		MsgDeferred("disabled")

	if calls != 0 {
		t.Fatalf("generators ran %d times for a disabled level, want 0", calls)
	}
	if buf.Len() != 0 {
		t.Fatalf("disabled event wrote %q", buf.String())
	}
}

// dropAllSampler samples out every event.
type dropAllSampler struct{}

func (dropAllSampler) Sample(lvl Level) bool { return false }

func TestDeferredSampledOutRunsZeroGenerators(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf).Sample(dropAllSampler{})

	calls := 0
	gen := okGen(&calls, 1)

	logger.Info().
		Defer("a", gen).
		Defer("b", gen).
		Defer("c", gen).
		MsgDeferred("sampled out")

	if calls != 0 {
		t.Fatalf("generators ran %d times for a sampled-out event, want 0", calls)
	}
	if buf.Len() != 0 {
		t.Fatalf("sampled-out event wrote %q", buf.String())
	}
}

func TestDeferredNoDeferredFieldsMatchesNormalPath(t *testing.T) {
	var a, b bytes.Buffer
	la, lb := New(&a), New(&b)
	la.Info().Str("k", "v").MsgDeferred("hello")
	lb.Info().Str("k", "v").Msg("hello")
	if a.String() != b.String() {
		t.Fatalf("MsgDeferred without deferred fields = %q, Msg = %q", a.String(), b.String())
	}
}

func ExampleEvent_MsgDeferred() {
	logger := New(os.Stdout)

	// Three deferred fields: the generators run only because the event is
	// enabled, in registration order, and the record is committed only
	// because all of them succeeded.
	logger.Info().
		Defer("user", func() (interface{}, error) { return "alice", nil }).
		Defer("action", func() (interface{}, error) { return "login", nil }).
		Defer("latency_ms", func() (interface{}, error) { return 12, nil }).
		MsgDeferred("request completed")

	// Output: {"level":"info","user":"alice","action":"login","latency_ms":12,"message":"request completed"}
}
