package zerolog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// --- Test fixture error types ---

// maliciousError counts how many times Error() is called and returns
// sensitive text. The safe encoder must never call Error() on it.
type maliciousError struct {
	calls int
}

func (e *maliciousError) Error() string {
	e.calls++
	return "sensitive-secret-text"
}

// wrappingError wraps an error and calls the wrapped error's Error() in its
// own Error() method (unlike fmt.wrapError which pre-builds the message).
type wrappingError struct {
	msg string
	err error
}

func (e *wrappingError) Error() string { return e.msg + ": " + e.err.Error() }
func (e *wrappingError) Unwrap() error { return e.err }

// selfRefError wraps itself, creating a cycle in the error chain.
type selfRefError struct {
	msg  string
	self error
}

func (e *selfRefError) Error() string { return e.msg }
func (e *selfRefError) Unwrap() error { return e.self }

// nodeError is a simple chain node for the three-node example.
type nodeError struct {
	msg   string
	cause error
}

func (e *nodeError) Error() string { return e.msg }
func (e *nodeError) Unwrap() error { return e.cause }

// unknownError is not registered as safe; only its type should be emitted.
type unknownError struct {
	secret string
}

func (e *unknownError) Error() string { return e.secret }

// safeError is registered as safe; its Error() text may be emitted verbatim.
type safeError struct {
	msg string
}

func (e *safeError) Error() string { return e.msg }

// extractorError has a registered SafeExtractor.
type extractorError struct {
	code int
}

func (e *extractorError) Error() string { return "should-not-appear" }

// --- helpers ---

func logSafeErr(t *testing.T, err error, cfg *SafeErrorConfig) string {
	t.Helper()
	out := &bytes.Buffer{}
	logger := New(out)
	if cfg == nil {
		logger.Log().SafeErr(err).Msg("msg")
	} else {
		logger.Log().SafeErrWith(err, cfg).Msg("msg")
	}
	return out.String()
}

func logSafeErrField(t *testing.T, key string, err error, cfg *SafeErrorConfig) string {
	t.Helper()
	out := &bytes.Buffer{}
	logger := New(out)
	if cfg == nil {
		logger.Log().SafeAnErr(key, err).Msg("msg")
	} else {
		logger.Log().SafeAnErrWith(key, err, cfg).Msg("msg")
	}
	return out.String()
}

// parseErrorField extracts the "error" object from a log line.
func parseErrorField(t *testing.T, line string) map[string]interface{} {
	t.Helper()
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		t.Fatalf("failed to parse log line: %v\nline: %s", err, line)
	}
	errObj, ok := doc["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected error field to be an object, got: %T\nline: %s", doc["error"], line)
	}
	return errObj
}

// causesOf returns the "causes" array of an error node.
func causesOf(t *testing.T, node map[string]interface{}) []interface{} {
	t.Helper()
	causes, ok := node["causes"].([]interface{})
	if !ok {
		t.Fatalf("expected causes to be an array, got: %T\nnode: %v", node["causes"], node)
	}
	return causes
}

// --- tests ---

// TestSafeErrJoinTwoCauses verifies that errors.Join with two errors
// produces two cause edges in the output.
func TestSafeErrJoinTwoCauses(t *testing.T) {
	err1 := errors.New("first")
	err2 := errors.New("second")
	joined := errors.Join(err1, err2)

	line := logSafeErr(t, joined, nil)
	node := parseErrorField(t, line)

	causes := causesOf(t, node)
	if len(causes) != 2 {
		t.Fatalf("expected 2 causes, got %d\nline: %s", len(causes), line)
	}
	// Each cause should be a node with a type.
	for i, c := range causes {
		cn, ok := c.(map[string]interface{})
		if !ok {
			t.Fatalf("cause %d: expected object, got %T", i, c)
		}
		if _, ok := cn["type"]; !ok {
			t.Fatalf("cause %d: missing type field\nnode: %v", i, cn)
		}
	}
}

// TestSafeErrMaliciousErrorNotCalled verifies that the safe encoder never
// calls Error() on an arbitrary (malicious) error, while the normal Err
// encoding does call it (showing the contrast).
func TestSafeErrMaliciousErrorNotCalled(t *testing.T) {
	me := &maliciousError{}
	err := &wrappingError{msg: "wrapped", err: me}

	// Safe encoding must not call Error().
	line := logSafeErr(t, err, nil)
	if me.calls != 0 {
		t.Fatalf("safe encoder called Error() %d times, want 0\nline: %s", me.calls, line)
	}
	// Sensitive text must not appear in output.
	if strings.Contains(line, "sensitive-secret-text") {
		t.Fatalf("sensitive text leaked in output\nline: %s", line)
	}

	// Normal Err encoding DOES call Error() (original behavior unchanged).
	out := &bytes.Buffer{}
	l := New(out)
	l.Log().Err(err).Msg("msg")
	if me.calls != 1 {
		t.Fatalf("normal Err() called Error() %d times, want 1\nline: %s", me.calls, out.String())
	}
}

// TestSafeErrSelfReferenceNoInfiniteExpand verifies that a self-referencing
// error does not cause infinite expansion and is marked as a cycle.
func TestSafeErrSelfReferenceNoInfiniteExpand(t *testing.T) {
	e := &selfRefError{msg: "root"}
	e.self = e

	line := logSafeErr(t, e, nil)
	node := parseErrorField(t, line)

	// The root should have an id (it is referenced by the self-loop).
	if _, ok := node["id"]; !ok {
		t.Fatalf("expected id on referenced root node\nnode: %v", node)
	}
	// The root should have a causes array with one entry.
	causes := causesOf(t, node)
	if len(causes) != 1 {
		t.Fatalf("expected 1 cause (self), got %d\nline: %s", len(causes), line)
	}
	// The self-reference should be marked as a cycle.
	refNode, ok := causes[0].(map[string]interface{})
	if !ok {
		t.Fatalf("self cause: expected object, got %T", causes[0])
	}
	if cycle, _ := refNode["cycle"].(bool); !cycle {
		t.Fatalf("expected cycle marker on self-reference\nnode: %v", refNode)
	}
	if _, ok := refNode["ref"]; !ok {
		t.Fatalf("expected ref field on self-reference\nnode: %v", refNode)
	}
}

// TestSafeErrDepthTruncationMarked verifies that when the depth limit is
// reached, the node is marked as truncated and the output does not pretend
// to be a complete cause chain.
func TestSafeErrDepthTruncationMarked(t *testing.T) {
	cfg := NewSafeErrorConfig(2)

	// Build a chain of 5 node errors.
	leaf := &nodeError{msg: "leaf"}
	n3 := &nodeError{msg: "n3", cause: leaf}
	n2 := &nodeError{msg: "n2", cause: n3}
	n1 := &nodeError{msg: "n1", cause: n2}
	root := &nodeError{msg: "root", cause: n1}

	line := logSafeErr(t, root, cfg)
	node := parseErrorField(t, line)

	// Walk the chain to find the truncated node.
	truncated := false
	cur := node
	for {
		if tr, _ := cur["truncated"].(string); tr == "depth" {
			truncated = true
			// The truncated node must not have a causes array.
			if _, ok := cur["causes"]; ok {
				t.Fatalf("truncated node should not have causes\nnode: %v", cur)
			}
			break
		}
		causes, ok := cur["causes"].([]interface{})
		if !ok || len(causes) == 0 {
			break
		}
		cur = causes[0].(map[string]interface{})
	}
	if !truncated {
		t.Fatalf("expected a truncated marker in the chain\nline: %s", line)
	}
}

// TestSafeErrUnknownOnlyType verifies that an error type not registered as
// safe only emits its type and level, not its message.
func TestSafeErrUnknownOnlyType(t *testing.T) {
	err := &unknownError{secret: "classified"}
	line := logSafeErr(t, err, nil)
	node := parseErrorField(t, line)

	if _, ok := node["type"]; !ok {
		t.Fatalf("missing type field\nnode: %v", node)
	}
	if _, ok := node["level"]; !ok {
		t.Fatalf("missing level field\nnode: %v", node)
	}
	if _, ok := node["message"]; ok {
		t.Fatalf("unknown error should not emit message\nnode: %v", node)
	}
	if strings.Contains(line, "classified") {
		t.Fatalf("secret text leaked\nline: %s", line)
	}
}

// TestSafeErrRegisteredSafeTypeEmitsMessage verifies that an error type
// registered as safe emits its verbatim Error() text.
func TestSafeErrRegisteredSafeTypeEmitsMessage(t *testing.T) {
	cfg := NewSafeErrorConfig(32)
	cfg.RegisterSafeTypeOf(&safeError{})

	err := &safeError{msg: "safe-message"}
	line := logSafeErr(t, err, cfg)
	node := parseErrorField(t, line)

	msg, ok := node["message"].(string)
	if !ok {
		t.Fatalf("expected message field\nnode: %v", node)
	}
	if msg != "safe-message" {
		t.Fatalf("expected message %q, got %q\nline: %s", "safe-message", msg, line)
	}
}

// TestSafeErrSafeExtractor verifies that a registered SafeExtractor is
// called and its fields are merged into the node, while Error() is not.
func TestSafeErrSafeExtractor(t *testing.T) {
	cfg := NewSafeErrorConfig(32)
	cfg.RegisterExtractor("*zerolog.extractorError", func(err error) map[string]interface{} {
		ee := err.(*extractorError)
		return map[string]interface{}{
			"code": ee.code,
		}
	})

	err := &extractorError{code: 42}
	line := logSafeErr(t, err, cfg)
	node := parseErrorField(t, line)

	code, ok := node["code"].(float64)
	if !ok {
		t.Fatalf("expected code field from extractor\nnode: %v", node)
	}
	if int(code) != 42 {
		t.Fatalf("expected code 42, got %v\nline: %s", code, line)
	}
	if _, ok := node["message"]; ok {
		t.Fatalf("extractor error should not emit verbatim message\nnode: %v", node)
	}
	if strings.Contains(line, "should-not-appear") {
		t.Fatalf("Error() text leaked\nline: %s", line)
	}
}

// TestSafeErrNil verifies that a nil error adds no field.
func TestSafeErrNil(t *testing.T) {
	line := logSafeErr(t, nil, nil)
	if strings.Contains(line, "error") {
		t.Fatalf("nil error should not add field\nline: %s", line)
	}
}

// TestSafeErrReuseByReference verifies that the same error instance reused
// in multiple branches is emitted as a reference (cycle marker) rather than
// being re-expanded.
func TestSafeErrReuseByReference(t *testing.T) {
	shared := &nodeError{msg: "shared"}
	err1 := &nodeError{msg: "branch1", cause: shared}
	err2 := &nodeError{msg: "branch2", cause: shared}
	joined := errors.Join(err1, err2)

	line := logSafeErr(t, joined, nil)
	node := parseErrorField(t, line)

	causes := causesOf(t, node)
	if len(causes) != 2 {
		t.Fatalf("expected 2 branches, got %d\nline: %s", len(causes), line)
	}
	// Walk both branches; the shared error should appear once fully (with an
	// id) and once as a reference (with cycle + ref).
	fullCount := 0
	refCount := 0
	for _, c := range causes {
		branch := c.(map[string]interface{})
		bc := causesOf(t, branch)
		if len(bc) != 1 {
			t.Fatalf("expected 1 cause per branch, got %d", len(bc))
		}
		sharedNode := bc[0].(map[string]interface{})
		if cycle, _ := sharedNode["cycle"].(bool); cycle {
			refCount++
			if _, ok := sharedNode["ref"]; !ok {
				t.Fatalf("expected ref field on reference node\nnode: %v", sharedNode)
			}
		} else {
			fullCount++
			if _, ok := sharedNode["id"]; !ok {
				t.Fatalf("expected id on fully-expanded shared node\nnode: %v", sharedNode)
			}
		}
	}
	if fullCount != 1 || refCount != 1 {
		t.Fatalf("expected 1 full expansion and 1 reference, got full=%d ref=%d\nline: %s", fullCount, refCount, line)
	}
}

// TestSafeErrCustomKey verifies that SafeAnErr uses the supplied key.
func TestSafeErrCustomKey(t *testing.T) {
	err := &safeError{msg: "m"}
	line := logSafeErrField(t, "myerr", err, nil)
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if _, ok := doc["myerr"]; !ok {
		t.Fatalf("expected field myerr\nline: %s", line)
	}
}

// --- Example ---

// ExampleEvent_SafeErr_threeNode logs a three-node error chain (root ->
// middle -> leaf) using the safe encoder. Only registered safe types emit
// verbatim text; the chain structure is preserved as cause edges.
func ExampleEvent_SafeErr_threeNode() {
	cfg := NewSafeErrorConfig(32)
	cfg.RegisterSafeTypeOf(&nodeError{})

	leaf := &nodeError{msg: "connection refused"}
	middle := &nodeError{msg: "failed to connect", cause: leaf}
	root := &nodeError{msg: "request failed", cause: middle}

	out := &bytes.Buffer{}
	logger := New(out)
	logger.Log().SafeErrWith(root, cfg).Msg("request failed")

	fmt.Print(out.String())
	// Output: {"error":{"type":"*zerolog.nodeError","level":0,"message":"request failed","causes":[{"type":"*zerolog.nodeError","level":1,"message":"failed to connect","causes":[{"type":"*zerolog.nodeError","level":2,"message":"connection refused"}]}]},"message":"request failed"}
}
