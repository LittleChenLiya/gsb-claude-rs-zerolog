package zerolog

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// NamespaceDupPolicy controls how a NamespaceEvent reacts when the same
// component writes a field key more than once.
type NamespaceDupPolicy int

const (
	// NamespaceDupReject rejects duplicate business fields: the second
	// write is dropped and an error is collected. If any error has been
	// collected, the whole event is refused at submit time (Msg/Send)
	// and nothing is written to the underlying writer.
	NamespaceDupReject NamespaceDupPolicy = iota
	// NamespaceDupKeepFirst silently keeps the first value written for a
	// duplicated key; subsequent writes to the same key are dropped.
	NamespaceDupKeepFirst
)

// NamespaceError is reported by NamespaceEvent.Err when a component
// violates the namespace rules (reserved field or duplicated key).
type NamespaceError struct {
	// Namespace is the component prefix the field was written under.
	Namespace string
	// Key is the business key (without the component prefix) the
	// component attempted to write.
	Key string
	// Reason explains why the field was refused.
	Reason error
}

func (e *NamespaceError) Error() string {
	return fmt.Sprintf("namespace %q: field %q rejected: %s", e.Namespace, e.Key, e.Reason)
}

var (
	errReservedField  = errors.New("reserved field name, only the logger core may set it")
	errDuplicateField = errors.New("duplicate field in the same namespace")
)

// isReservedFieldName reports whether key is owned by the logger core and
// must never be overwritten by a component namespace. The check compares
// against the configurable global field names so customizations are honored.
func isReservedFieldName(key string) bool {
	switch key {
	case TimestampFieldName,
		LevelFieldName,
		MessageFieldName,
		ErrorFieldName,
		ErrorStackFieldName,
		CallerFieldName:
		return true
	}
	return false
}

// NamespaceEvent is an explicitly enabled wrapper around *Event that isolates
// fields written by different components. Every business field is emitted in
// a per-component object ("component":{...}) so two components can safely
// write the same key (e.g. "id" is emitted as "db":{"id":...} and
// "http":{"id":...}, i.e. db.id and http.id in dotted notation).
//
// Reserved fields (time, level, message, error, stack, caller and their
// customized equivalents) are refused to components.
//
// The wrapper is opt-in: a plain *Event keeps its allocation-free hot path;
// a NamespaceEvent is only created when Event.Namespace is called. When the
// wrapped event is disabled (filtered level or sampler), Namespace returns a
// lightweight, disabled wrapper whose field methods are no-ops, so expensive
// field construction is skipped just like the Event.Func contract.
//
// Validation is deferred until the message is submitted (Msg/Msgf/Send):
// if validation fails, no half-built log line is ever written to the
// underlying writer.
type NamespaceEvent struct {
	parent *Event // nil for a disabled wrapper
	// component is the current component prefix; "" for the top-level
	// namespace (fields written before the first Component call).
	component string
	policy    NamespaceDupPolicy
	// scratch holds fields that still belong to an open component
	// namespace. It is a complete JSON object body starting with '{';
	// it is spliced into the parent event at component boundaries or at
	// submit time.
	scratch *Event
	// seen tracks the keys already used by the currently open component
	// (or top-level) block, to enforce the duplicate policy.
	seen map[string]struct{}
	errs []error
	// submitted prevents the wrapped event from being flushed twice.
	submitted bool
}

// Namespace turns the event into an explicitly namespaced event. Field
// names are isolated per component, reserved fields are protected and
// duplicate fields are handled according to policy.
//
// On a nil (disabled) event a disabled wrapper is returned: all methods
// are no-ops and Enabled reports false, so callers can skip expensive
// field construction. Sampling and disabled-level behavior of the parent
// logger is fully preserved.
func (e *Event) Namespace(policy NamespaceDupPolicy) *NamespaceEvent {
	if e == nil || !e.Enabled() {
		return &NamespaceEvent{policy: policy}
	}
	return &NamespaceEvent{
		parent: e,
		policy: policy,
		seen:   make(map[string]struct{}),
	}
}

// Enabled reports whether the wrapped event will actually be emitted.
func (n *NamespaceEvent) Enabled() bool {
	return n != nil && n.parent != nil
}

// Component opens a new component namespace. Fields written afterwards are
// emitted inside the "component" object (component.key in dotted notation).
// Any fields written before the first Component call are attached directly
// to the event (they must still obey the reserved-field and duplicate
// rules).
func (n *NamespaceEvent) Component(name string) *NamespaceEvent {
	if n == nil || n.parent == nil {
		return n
	}
	n.flushScratch()
	n.component = name
	n.seen = make(map[string]struct{})
	return n
}

// flushScratch splices the currently open component block into the parent
// event under the "component":{...} object, or, for the top-level block,
// merges the fields directly into the event.
func (n *NamespaceEvent) flushScratch() {
	if n.scratch == nil {
		return
	}
	body := n.scratch.buf // starts with '{', no end marker yet
	if n.component == "" {
		// Merge top-level fields directly into the parent object.
		n.parent.buf = enc.AppendObjectData(n.parent.buf, body)
	} else {
		n.parent.buf = enc.AppendKey(n.parent.buf, n.component)
		n.parent.buf = append(n.parent.buf, body...)
		n.parent.buf = enc.AppendEndMarker(n.parent.buf)
	}
	putEvent(n.scratch)
	n.scratch = nil
}

// ensureScratch lazily creates the buffer that holds the current component
// block. The leading '{' added by newEvent is kept so AppendObjectData can
// be used when splicing.
func (n *NamespaceEvent) ensureScratch() *Event {
	if n.scratch == nil {
		n.scratch = newEvent(LevelWriterAdapter{io.Discard}, DebugLevel, n.parent.stack, n.parent.ctx, nil)
	}
	return n.scratch
}

// check validates a component field write without touching any buffer. It
// returns false when the field must be skipped (reserved key, duplicate or
// disabled event). Errors are collected for the submit-time validation.
func (n *NamespaceEvent) check(key string) bool {
	if n == nil || n.parent == nil {
		return false
	}
	if isReservedFieldName(key) {
		n.errs = append(n.errs, &NamespaceError{
			Namespace: n.component,
			Key:       key,
			Reason:    errReservedField,
		})
		return false
	}
	if _, dup := n.seen[key]; dup {
		if n.policy == NamespaceDupReject {
			n.errs = append(n.errs, &NamespaceError{
				Namespace: n.component,
				Key:       key,
				Reason:    errDuplicateField,
			})
		}
		return false
	}
	n.seen[key] = struct{}{}
	return true
}

// finalize runs the unified, submit-time validation. It returns true only
// when the event is valid and the wrapped event is written. On failure the
// collected fields are discarded and the underlying writer is never called,
// so no half log line can be emitted.
func (n *NamespaceEvent) finalize(msg string) bool {
	if n == nil || n.parent == nil || n.submitted {
		return false
	}
	n.submitted = true
	if len(n.errs) > 0 {
		// Drop the whole event, including any buffered component blocks.
		if n.scratch != nil {
			putEvent(n.scratch)
			n.scratch = nil
		}
		for _, err := range n.errs {
			if ErrorHandler != nil {
				ErrorHandler(err)
			} else {
				fmt.Fprintf(os.Stderr, "zerolog: %v\n", err)
			}
		}
		// Dispose the parent event without writing it.
		n.parent.level = Disabled
		n.parent.Msg(msg)
		return false
	}
	n.flushScratch()
	n.parent.Msg(msg)
	return true
}

// Msg submits the event after validating every namespaced field. If
// validation failed, nothing is written to the underlying writer.
func (n *NamespaceEvent) Msg(msg string) {
	n.finalize(msg)
}

// Send is equivalent to Msg("").
func (n *NamespaceEvent) Send() {
	n.finalize("")
}

// Msgf submits the event with a formatted message.
func (n *NamespaceEvent) Msgf(format string, v ...interface{}) {
	n.finalize(fmt.Sprintf(format, v...))
}

// Err returns the validation errors collected so far (reserved field
// attempts or, with NamespaceDupReject, duplicate fields). The return is
// nil when every field was accepted.
func (n *NamespaceEvent) Err() error {
	if n == nil || len(n.errs) == 0 {
		return nil
	}
	return errors.Join(n.errs...)
}

// Str adds a string field to the current component namespace.
func (n *NamespaceEvent) Str(key, val string) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Str(key, val)
	}
	return n
}

// Bytes adds a byte-slice field to the current component namespace.
func (n *NamespaceEvent) Bytes(key string, val []byte) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Bytes(key, val)
	}
	return n
}

// Bool adds a bool field to the current component namespace.
func (n *NamespaceEvent) Bool(key string, b bool) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Bool(key, b)
	}
	return n
}

// Int adds an int field to the current component namespace.
func (n *NamespaceEvent) Int(key string, i int) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Int(key, i)
	}
	return n
}

// Int64 adds an int64 field to the current component namespace.
func (n *NamespaceEvent) Int64(key string, i int64) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Int64(key, i)
	}
	return n
}

// Uint64 adds a uint64 field to the current component namespace.
func (n *NamespaceEvent) Uint64(key string, i uint64) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Uint64(key, i)
	}
	return n
}

// Float64 adds a float64 field to the current component namespace.
func (n *NamespaceEvent) Float64(key string, f float64) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Float64(key, f)
	}
	return n
}

// Time adds a time.Time field to the current component namespace.
func (n *NamespaceEvent) Time(key string, t time.Time) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Time(key, t)
	}
	return n
}

// Dur adds a duration field to the current component namespace.
func (n *NamespaceEvent) Dur(key string, d time.Duration) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Dur(key, d)
	}
	return n
}

// Interface adds a field via reflection to the current component namespace.
func (n *NamespaceEvent) Interface(key string, i interface{}) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Interface(key, i)
	}
	return n
}

// Any is an alias for Interface.
func (n *NamespaceEvent) Any(key string, i interface{}) *NamespaceEvent {
	return n.Interface(key, i)
}

// Stringer adds a fmt.Stringer field to the current component namespace.
func (n *NamespaceEvent) Stringer(key string, val fmt.Stringer) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Stringer(key, val)
	}
	return n
}

// IPAddr adds a net.IP field to the current component namespace.
func (n *NamespaceEvent) IPAddr(key string, ip net.IP) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().IPAddr(key, ip)
	}
	return n
}

// AnErr adds a serialized error field to the current component namespace.
// The reserved core key "error" is only reachable through Event.Err; a
// component must use a business key here.
func (n *NamespaceEvent) AnErr(key string, err error) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().AnErr(key, err)
	}
	return n
}

// Strs adds a []string field to the current component namespace.
func (n *NamespaceEvent) Strs(key string, vals []string) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Strs(key, vals)
	}
	return n
}

// Ints adds a []int field to the current component namespace.
func (n *NamespaceEvent) Ints(key string, vals []int) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Ints(key, vals)
	}
	return n
}

// Array adds an array field to the current component namespace. Nested
// content keeps the component namespace because it is emitted inside the
// component object.
func (n *NamespaceEvent) Array(key string, arr LogArrayMarshaler) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Array(key, arr)
	}
	return n
}

// Object adds a nested object field to the current component namespace.
// The nested object keeps its own (nested) namespace inside the component
// object: its business keys never collide with other components or with
// core reserved fields, and it is serialized only for enabled, accepted
// events.
func (n *NamespaceEvent) Object(key string, obj LogObjectMarshaler) *NamespaceEvent {
	if n.check(key) {
		n.ensureScratch().Object(key, obj)
	}
	return n
}

// Func runs f only when the wrapped event is enabled, so expensive field
// construction is skipped for filtered or sampled-out events.
func (n *NamespaceEvent) Func(f func(n *NamespaceEvent)) *NamespaceEvent {
	if n != nil && n.Enabled() {
		f(n)
	}
	return n
}
