package zerolog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// maliciousError counts every call to Error and to MarshalJSON so tests can
// prove the safe encoder never reads sensitive text.
type maliciousError struct {
	errorCalls int
	jsonCalls  int
	secret     string
}

func (m *maliciousError) Error() string {
	m.errorCalls++
	return "SECRET-LEAK:" + m.secret
}

func (m *maliciousError) MarshalJSON() ([]byte, error) {
	m.jsonCalls++
	return []byte(`"` + m.secret + `"`), nil
}

// maliciousWrapper wraps a cause and also leaks when Error is called.
type maliciousWrapper struct {
	cause error
}

func (m *maliciousWrapper) Error() string { return "WRAPPER-LEAK" }
func (m *maliciousWrapper) Unwrap() error { return m.cause }

// selfError can point at itself to build a cyclic chain.
type selfError struct {
	name string
	next error
}

func (s *selfError) Error() string { return s.name }
func (s *selfError) Unwrap() error { return s.next }

// safeCodeError is a registered error exposing a safe code via an extractor.
type safeCodeError struct {
	code    int
	message string // potentially sensitive; the extractor must not read it
}

func (s *safeCodeError) Error() string { return s.message }

// multiWrap wraps two errors using a custom multi-cause type.
type multiWrap struct {
	errs []error
}

func (m *multiWrap) Error() string   { return "multi" }
func (m *multiWrap) Unwrap() []error { return m.errs }

// linkError forms long linear chains.
type linkError struct {
	n     int
	cause error
}

func (l *linkError) Error() string { return fmt.Sprintf("link-%d", l.n) }
func (l *linkError) Unwrap() error { return l.cause }

func deepChain(n int) error {
	var err error = &linkError{n: 0}
	for i := 1; i < n; i++ {
		err = &linkError{n: i, cause: err}
	}
	return err
}

func timeoutAfter() <-chan time.Time { return time.After(2 * time.Second) }

func renderChain(t *testing.T, err error, opts ...ErrorChainOption) map[string]interface{} {
	t.Helper()
	out := &bytes.Buffer{}
	logger := New(out)
	logger.Log().ErrChain("error", err, opts...).Msg("msg")
	var got map[string]interface{}
	if jerr := json.Unmarshal(out.Bytes(), &got); jerr != nil {
		t.Fatalf("invalid json %q: %v", out.String(), jerr)
	}
	return got
}

func TestSafeErrorChainNeverCallsErrorOrMarshalJSON(t *testing.T) {
	mal := &maliciousError{secret: "topsecret"}
	wrapped := &maliciousWrapper{cause: mal}

	got := renderChain(t, wrapped)

	if mal.errorCalls != 0 {
		t.Errorf("Error() called %d times, want 0", mal.errorCalls)
	}
	if mal.jsonCalls != 0 {
		t.Errorf("MarshalJSON() called %d times, want 0", mal.jsonCalls)
	}
	if strings.Contains(fmt.Sprintf("%v", got), "topsecret") {
		t.Errorf("sensitive text leaked into output: %v", got)
	}
	if strings.Contains(fmt.Sprintf("%v", got), "SECRET-LEAK") {
		t.Errorf("Error() text leaked into output: %v", got)
	}
}

func TestSafeErrorChainJoinHasTwoCauses(t *testing.T) {
	a := &maliciousError{secret: "a"}
	b := &maliciousError{secret: "b"}
	joined := errors.Join(a, b)

	chain := NewErrorChain(joined)

	if !chain.hasRoot || len(chain.nodes) != 3 {
		t.Fatalf("want root + 2 cause nodes, got %d", len(chain.nodes))
	}
	if len(chain.edges) != 2 {
		t.Fatalf("errors.Join must keep two cause edges, got %d: %+v", len(chain.edges), chain.edges)
	}
	tos := map[int]bool{}
	for _, e := range chain.edges {
		if e.from != chain.root {
			t.Errorf("edge must originate at root %d, got %d", chain.root, e.from)
		}
		tos[e.to] = true
	}
	if len(tos) != 2 {
		t.Errorf("want two distinct causes, got %v", tos)
	}
	if a.errorCalls != 0 || b.errorCalls != 0 {
		t.Errorf("malicious Error called: a=%d b=%d", a.errorCalls, b.errorCalls)
	}
}

func TestSafeErrorChainSelfReferenceDoesNotLoop(t *testing.T) {
	cyclic := &selfError{name: "cyc"}
	cyclic.next = cyclic

	done := make(chan struct{})
	var chain *ErrorChain
	go func() {
		chain = NewErrorChain(cyclic)
		close(done)
	}()
	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("self-referential chain expansion did not terminate")
	}

	if len(chain.nodes) != 1 {
		t.Fatalf("self reference must reuse one node by identity, got %d nodes", len(chain.nodes))
	}
	if len(chain.edges) != 1 {
		t.Fatalf("self reference yields one self edge, got %d", len(chain.edges))
	}
	if chain.edges[0].from != chain.edges[0].to {
		t.Errorf("self edge should point the node to itself")
	}
}

func TestSafeErrorChainReusedCauseIsReferencedOnce(t *testing.T) {
	shared := &linkError{n: 100}
	m := &multiWrap{errs: []error{shared, shared}}

	chain := NewErrorChain(m)

	if len(chain.nodes) != 2 {
		t.Fatalf("same error reused must resolve to one node, got %d", len(chain.nodes))
	}
	if len(chain.edges) != 1 {
		t.Fatalf("duplicate cause edges should be de-duplicated, got %d", len(chain.edges))
	}
}

func TestSafeErrorChainDepthIsExplicitlyTruncated(t *testing.T) {
	err := deepChain(10)
	chain := NewErrorChain(err, ErrorChainWithMaxDepth(3))

	if !chain.truncated {
		t.Errorf("chain beyond depth limit must be marked truncated")
	}
	if len(chain.nodes) != 4 {
		t.Fatalf("want 4 nodes (layers 0..3), got %d", len(chain.nodes))
	}
	if !chain.nodes[3].truncated {
		t.Errorf("deepest node must carry truncated flag")
	}

	got := renderChain(t, err, ErrorChainWithMaxDepth(3))
	errObj := got["error"].(map[string]interface{})
	if errObj["truncated"] != true {
		t.Errorf("output must expose truncated:true, got %v", errObj["truncated"])
	}
}

func TestSafeErrorChainUnknownReportsTypeOnly(t *testing.T) {
	mal := &maliciousError{secret: "zzz"}
	got := renderChain(t, mal)
	errObj := got["error"].(map[string]interface{})
	node := errObj["nodes"].([]interface{})[0].(map[string]interface{})
	if node["category"] != "unknown" {
		t.Errorf("unregistered type category=unknown, got %v", node["category"])
	}
	typeName, _ := node["type"].(string)
	if !strings.Contains(typeName, "maliciousError") {
		t.Errorf("unknown node must report its type, got %v", node["type"])
	}
	if mal.errorCalls != 0 {
		t.Errorf("Error called %d times", mal.errorCalls)
	}
}

func TestSafeErrorChainRegisteredCategoryAndOptionalExtractor(t *testing.T) {
	RegisterSafeErrorType(&safeCodeError{}, "safe_code", func(err error) map[string]interface{} {
		s := err.(*safeCodeError)
		return map[string]interface{}{"code": s.code}
	})

	se := &safeCodeError{code: 42, message: "do-not-read-me"}
	got := renderChain(t, se)
	node := got["error"].(map[string]interface{})["nodes"].([]interface{})[0].(map[string]interface{})
	if node["category"] != "safe_code" {
		t.Errorf("registered category, got %v", node["category"])
	}
	fields := node["fields"].(map[string]interface{})
	if fields["code"].(float64) != 42 {
		t.Errorf("extractor field, got %v", fields["code"])
	}
	if strings.Contains(fmt.Sprintf("%v", got), "do-not-read-me") {
		t.Errorf("sensitive message must not appear")
	}

	gotNoFields := renderChain(t, se, ErrorChainWithoutExtractors())
	nodeNoFields := gotNoFields["error"].(map[string]interface{})["nodes"].([]interface{})[0].(map[string]interface{})
	if _, ok := nodeNoFields["fields"]; ok {
		t.Errorf("fields must be omitted when extractors are disabled")
	}
}

func TestSafeErrorChainNilRendersNullAndDefaultErrUnchanged(t *testing.T) {
	out := &bytes.Buffer{}
	logger := New(out)
	logger.Log().ErrChain("error", nil).Msg("m")
	if !strings.Contains(out.String(), `"error":null`) {
		t.Errorf("nil chain must render null, got %s", out.String())
	}

	out2 := &bytes.Buffer{}
	l2 := New(out2)
	l2.Log().Err(errors.New("plain-text-err")).Msg("m")
	if !strings.Contains(out2.String(), `"error":"plain-text-err"`) {
		t.Errorf("default Err encoding changed: %s", out2.String())
	}
}

func TestSafeErrorChainThreeNodeGraphShape(t *testing.T) {
	root := &maliciousWrapper{cause: &maliciousWrapper{cause: &maliciousError{secret: "x"}}}
	chain := NewErrorChain(root)
	if len(chain.nodes) != 3 {
		t.Fatalf("want three nodes, got %d", len(chain.nodes))
	}
	if chain.root != 0 {
		t.Errorf("root id must be 0")
	}
	wantEdges := map[[2]int]bool{{0, 1}: false, {1, 2}: false}
	for _, e := range chain.edges {
		wantEdges[[2]int{e.from, e.to}] = true
	}
	for k, v := range wantEdges {
		if !v {
			t.Errorf("missing edge %v", k)
		}
	}
	layers := []int{chain.nodes[0].layer, chain.nodes[1].layer, chain.nodes[2].layer}
	if !reflect.DeepEqual(layers, []int{0, 1, 2}) {
		t.Errorf("layers want [0 1 2], got %v", layers)
	}
}

func TestSafeErrorChainDiamondReusesIdentityAcrossBranches(t *testing.T) {
	shared := &linkError{n: 7}
	left := &linkError{n: 1, cause: shared}
	right := &linkError{n: 2, cause: shared}
	joined := errors.Join(left, right)

	chain := NewErrorChain(joined)

	// Nodes: join root, left, right, shared => 4 unique identities.
	if len(chain.nodes) != 4 {
		t.Fatalf("shared cause must be one node, got %d", len(chain.nodes))
	}
	// Edges: root->left, root->right, left->shared, right->shared => 4.
	if len(chain.edges) != 4 {
		t.Fatalf("diamond must keep four edges, got %d: %+v", len(chain.edges), chain.edges)
	}
	sharedID := -1
	refs := 0
	for _, e := range chain.edges {
		if e.to != chain.root {
			// count inbound edges to the shared node
			if chain.nodes[e.to].typeName == typeName(reflect.TypeOf(shared)) && chain.nodes[e.to].layer == 2 {
				if sharedID == -1 {
					sharedID = e.to
				}
				if e.to == sharedID {
					refs++
				}
			}
		}
	}
	if refs != 2 {
		t.Errorf("shared node must be referenced by both branches, got %d inbound edges", refs)
	}
}
