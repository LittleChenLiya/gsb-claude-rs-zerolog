package zerolog_test

import (
	"os"

	zerolog "github.com/rs/zerolog"
)

// exampleNodeError is a three-level error chain. Its Error method is never
// called by the safe encoder.
type exampleNodeError struct {
	label string
	cause error
}

func (e *exampleNodeError) Error() string { return e.label }
func (e *exampleNodeError) Unwrap() error { return e.cause }

// ExampleErrorChain prints the safe encoding of a three-node chain: a root
// wrapping a middle node wrapping a leaf. The output only carries registered
// categories, layer numbers, type names and cause edges; no error text is read.
//
// To reproduce:
//
//	go test -run ExampleErrorChain -count=1 .
func ExampleErrorChain() {
	// Declare which categories are safe to report. Unknown types are reported
	// by type name only.
	zerolog.RegisterSafeErrorType(&exampleNodeError{}, "node", nil)

	leaf := &exampleNodeError{label: "leaf-sensitive-text"}
	mid := &exampleNodeError{label: "mid-sensitive-text", cause: leaf}
	root := &exampleNodeError{label: "root-sensitive-text", cause: mid}

	logger := zerolog.New(os.Stdout)
	logger.Log().ErrChain("error", root).Msg("request failed")
	// Output:
	// {"error":{"root":0,"nodes":[{"id":0,"type":"*zerolog_test.exampleNodeError","category":"node","layer":0},{"id":1,"type":"*zerolog_test.exampleNodeError","category":"node","layer":1},{"id":2,"type":"*zerolog_test.exampleNodeError","category":"node","layer":2}],"edges":[{"from":0,"to":1},{"from":1,"to":2}]},"message":"request failed"}
}
