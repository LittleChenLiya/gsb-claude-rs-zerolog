package zerolog

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
)

// DuplicateFieldStrategy defines how duplicate field keys within the same
// namespace are handled by a NamespacedEvent.
type DuplicateFieldStrategy int

const (
	// DuplicateFieldReject causes a duplicate field in the same namespace to
	// be rejected: the event is not written to the underlying writer and an
	// error is reported via the global ErrorHandler (or stderr).
	DuplicateFieldReject DuplicateFieldStrategy = iota
	// DuplicateFieldKeepFirst keeps the first value written for a key and
	// silently ignores subsequent writes of the same key within the namespace.
	DuplicateFieldKeepFirst
)

// NamespaceOption configures a NamespacedEvent.
type NamespaceOption func(*NamespacedEvent)

// WithDuplicateStrategy sets the strategy used to handle duplicate fields
// within the same namespace. The default is DuplicateFieldReject.
func WithDuplicateStrategy(s DuplicateFieldStrategy) NamespaceOption {
	return func(n *NamespacedEvent) {
		n.strategy = s
	}
}

// NamespacedLogObjectMarshaler is implemented by types that can marshal
// themselves into a NamespacedEvent. Unlike LogObjectMarshaler, fields written
// through this interface are prefixed with the full namespace path (e.g.
// "db.stats.rows"), flattening the object into the component namespace
// rather than nesting it under a sub-object.
type NamespacedLogObjectMarshaler interface {
	MarshalZerologNamespacedObject(e *NamespacedEvent)
}

// NamespacedEvent wraps an *Event and prefixes every field name with a
// component namespace so that fields written by different components do not
// collide. It is an opt-in wrapper: the regular Event hot path is unchanged.
//
// Reserved fields (timestamp, level, message) cannot be overwritten by a
// component. Duplicate fields within the same namespace are handled according
// to the configured DuplicateFieldStrategy. The event is validated before it
// is committed, so a failed validation never writes a partial log line to the
// underlying writer.
//
// Nested objects keep their own namespace: an object added under key "stats"
// from component "db" is rendered as a nested object under "db.stats" (or, if
// the object implements NamespacedLogObjectMarshaler, as flattened fields
// "db.stats.<field>").
type NamespacedEvent struct {
	e        *Event
	prefix   string
	strategy DuplicateFieldStrategy
	seen     map[string]struct{}
	err      error
}

// Namespace wraps e with the given component namespace. Field names written
// through the returned wrapper are prefixed with "<component>.". The wrapper
// is only active when explicitly used; the underlying Event hot path is not
// modified.
func (e *Event) Namespace(component string, opts ...NamespaceOption) *NamespacedEvent {
	n := &NamespacedEvent{
		e:        e,
		prefix:   component + ".",
		strategy: DuplicateFieldReject,
		seen:     make(map[string]struct{}),
	}
	for _, opt := range opts {
		opt(n)
	}
	return n
}

// sub returns a child wrapper with the prefix extended by key. The child
// shares the parent duplicate-tracking map (keyed by full prefixed key) so
// that duplicates are detected across the whole event.
func (n *NamespacedEvent) sub(key string) *NamespacedEvent {
	return &NamespacedEvent{
		e:        n.e,
		prefix:   n.prefix + key + ".",
		strategy: n.strategy,
		seen:     n.seen,
	}
}

// isReservedField reports whether key is a reserved log field name that
// components are not allowed to overwrite.
func isReservedField(key string) bool {
	return key == TimestampFieldName || key == LevelFieldName || key == MessageFieldName
}

// check validates a field key and returns the prefixed key to write. It
// returns the empty string if the field should be skipped (reserved field,
// duplicate with KeepFirst strategy, or event disabled). It sets n.err if the
// field is rejected.
func (n *NamespacedEvent) check(key string) string {
	if n == nil || n.e == nil || !n.e.Enabled() || n.err != nil {
		return ""
	}
	if isReservedField(key) {
		n.err = fmt.Errorf("zerolog: namespaced event %q: field %q is reserved and cannot be set by a component", n.prefix, key)
		return ""
	}
	full := n.prefix + key
	if _, exists := n.seen[full]; exists {
		switch n.strategy {
		case DuplicateFieldReject:
			n.err = fmt.Errorf("zerolog: namespaced event %q: duplicate field %q", n.prefix, full)
			return ""
		case DuplicateFieldKeepFirst:
			return ""
		}
	}
	n.seen[full] = struct{}{}
	return full
}

// Enabled returns false if the underlying event is filtered out by the log
// level or sampling.
func (n *NamespacedEvent) Enabled() bool {
	return n != nil && n.e != nil && n.e.Enabled()
}

// Discard disables the event so Msg(f) won't print it.
func (n *NamespacedEvent) Discard() *NamespacedEvent {
	if n == nil || n.e == nil {
		return n
	}
	n.e.Discard()
	return nil
}

// Msg sends the namespaced event with msg added as the message field if not
// empty. If validation failed, the event is discarded without writing to the
// underlying writer and the error is reported via the global ErrorHandler (or
// stderr).
func (n *NamespacedEvent) Msg(msg string) {
	if n == nil {
		return
	}
	if n.err != nil {
		n.fail()
		return
	}
	n.e.Msg(msg)
}

// Send is equivalent to calling Msg("").
func (n *NamespacedEvent) Send() {
	if n == nil {
		return
	}
	if n.err != nil {
		n.fail()
		return
	}
	n.e.Send()
}

// Msgf sends the namespaced event with formatted msg added as the message
// field if not empty.
func (n *NamespacedEvent) Msgf(format string, v ...interface{}) {
	if n == nil {
		return
	}
	if n.err != nil {
		n.fail()
		return
	}
	n.e.Msgf(format, v...)
}

// MsgFunc sends the namespaced event with the message returned by createMsg.
// createMsg is not called if the event is disabled or validation failed.
func (n *NamespacedEvent) MsgFunc(createMsg func() string) {
	if n == nil {
		return
	}
	if n.err != nil {
		n.fail()
		return
	}
	if !n.e.Enabled() {
		return
	}
	n.e.MsgFunc(createMsg)
}

// fail discards the event and reports the validation error. Nothing is written
// to the underlying writer.
func (n *NamespacedEvent) fail() {
	if n.e != nil {
		n.e.level = Disabled
		putEvent(n.e)
	}
	if ErrorHandler != nil {
		ErrorHandler(n.err)
	} else {
		fmt.Fprintf(os.Stderr, "zerolog: %v\n", n.err)
	}
}

// Func allows an anonymous func to run only if the underlying event is
// enabled. This prevents expensive field construction from running for
// disabled events.
func (n *NamespacedEvent) Func(f func(e *NamespacedEvent)) *NamespacedEvent {
	if n != nil && n.e != nil && n.e.Enabled() && n.err == nil {
		f(n)
	}
	return n
}

// Str adds the field key with val as a string to the namespaced event.
func (n *NamespacedEvent) Str(key, val string) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Str(full, val)
	}
	return n
}

// Strs adds the field key with vals as a []string to the namespaced event.
func (n *NamespacedEvent) Strs(key string, vals []string) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Strs(full, vals)
	}
	return n
}

// StrsV adds the field key with vals as a []string to the namespaced event.
func (n *NamespacedEvent) StrsV(key string, vals ...string) *NamespacedEvent {
	return n.Strs(key, vals)
}

// Stringer adds the field key and a val to the namespaced event.
func (n *NamespacedEvent) Stringer(key string, val fmt.Stringer) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Stringer(full, val)
	}
	return n
}

// Stringers adds the field key with vals to the namespaced event.
func (n *NamespacedEvent) Stringers(key string, vals []fmt.Stringer) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Stringers(full, vals)
	}
	return n
}

// StringersV adds the field key with vals to the namespaced event.
func (n *NamespacedEvent) StringersV(key string, vals ...fmt.Stringer) *NamespacedEvent {
	return n.Stringers(key, vals)
}

// Bytes adds the field key with val as a string to the namespaced event.
func (n *NamespacedEvent) Bytes(key string, val []byte) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Bytes(full, val)
	}
	return n
}

// Hex adds the field key with val as a hex string to the namespaced event.
func (n *NamespacedEvent) Hex(key string, val []byte) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Hex(full, val)
	}
	return n
}

// RawJSON adds already encoded JSON to the log line under key.
func (n *NamespacedEvent) RawJSON(key string, b []byte) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.RawJSON(full, b)
	}
	return n
}

// RawCBOR adds already encoded CBOR to the log line under key.
func (n *NamespacedEvent) RawCBOR(key string, b []byte) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.RawCBOR(full, b)
	}
	return n
}

// AnErr adds the field key with serialized err to the namespaced event.
// If err is nil, no field is added.
func (n *NamespacedEvent) AnErr(key string, err error) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.AnErr(full, err)
	}
	return n
}

// Errs adds the field key with errs as an array of serialized errors to the
// namespaced event.
func (n *NamespacedEvent) Errs(key string, errs []error) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Errs(full, errs)
	}
	return n
}

// Err adds the field "error" (namespaced) with serialized err to the event.
// If err is nil, no field is added.
func (n *NamespacedEvent) Err(err error) *NamespacedEvent {
	if full := n.check(ErrorFieldName); full != "" {
		n.e.AnErr(full, err)
	}
	return n
}

// Bool adds the field key with val as a bool to the namespaced event.
func (n *NamespacedEvent) Bool(key string, b bool) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Bool(full, b)
	}
	return n
}

// Bools adds the field key with val as a []bool to the namespaced event.
func (n *NamespacedEvent) Bools(key string, b []bool) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Bools(full, b)
	}
	return n
}

// Int adds the field key with i as a int to the namespaced event.
func (n *NamespacedEvent) Int(key string, i int) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Int(full, i)
	}
	return n
}

// Ints adds the field key with i as a []int to the namespaced event.
func (n *NamespacedEvent) Ints(key string, i []int) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Ints(full, i)
	}
	return n
}

// IntsV adds the field key with i as a []int to the namespaced event.
func (n *NamespacedEvent) IntsV(key string, i ...int) *NamespacedEvent {
	return n.Ints(key, i)
}

// Int8 adds the field key with i as a int8 to the namespaced event.
func (n *NamespacedEvent) Int8(key string, i int8) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Int8(full, i)
	}
	return n
}

// Ints8 adds the field key with i as a []int8 to the namespaced event.
func (n *NamespacedEvent) Ints8(key string, i []int8) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Ints8(full, i)
	}
	return n
}

// Ints8V adds the field key with i as a []int8 to the namespaced event.
func (n *NamespacedEvent) Ints8V(key string, i ...int8) *NamespacedEvent {
	return n.Ints8(key, i)
}

// Int16 adds the field key with i as a int16 to the namespaced event.
func (n *NamespacedEvent) Int16(key string, i int16) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Int16(full, i)
	}
	return n
}

// Ints16 adds the field key with i as a []int16 to the namespaced event.
func (n *NamespacedEvent) Ints16(key string, i []int16) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Ints16(full, i)
	}
	return n
}

// Ints16V adds the field key with i as a []int16 to the namespaced event.
func (n *NamespacedEvent) Ints16V(key string, i ...int16) *NamespacedEvent {
	return n.Ints16(key, i)
}

// Int32 adds the field key with i as a int32 to the namespaced event.
func (n *NamespacedEvent) Int32(key string, i int32) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Int32(full, i)
	}
	return n
}

// Ints32 adds the field key with i as a []int32 to the namespaced event.
func (n *NamespacedEvent) Ints32(key string, i []int32) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Ints32(full, i)
	}
	return n
}

// Ints32V adds the field key with i as a []int32 to the namespaced event.
func (n *NamespacedEvent) Ints32V(key string, i ...int32) *NamespacedEvent {
	return n.Ints32(key, i)
}

// Int64 adds the field key with i as a int64 to the namespaced event.
func (n *NamespacedEvent) Int64(key string, i int64) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Int64(full, i)
	}
	return n
}

// Ints64 adds the field key with i as a []int64 to the namespaced event.
func (n *NamespacedEvent) Ints64(key string, i []int64) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Ints64(full, i)
	}
	return n
}

// Ints64V adds the field key with i as a []int64 to the namespaced event.
func (n *NamespacedEvent) Ints64V(key string, i ...int64) *NamespacedEvent {
	return n.Ints64(key, i)
}

// Uint adds the field key with i as a uint to the namespaced event.
func (n *NamespacedEvent) Uint(key string, i uint) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Uint(full, i)
	}
	return n
}

// Uints adds the field key with i as a []uint to the namespaced event.
func (n *NamespacedEvent) Uints(key string, i []uint) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Uints(full, i)
	}
	return n
}

// UintsV adds the field key with i as a []uint to the namespaced event.
func (n *NamespacedEvent) UintsV(key string, i ...uint) *NamespacedEvent {
	return n.Uints(key, i)
}

// Uint8 adds the field key with i as a uint8 to the namespaced event.
func (n *NamespacedEvent) Uint8(key string, i uint8) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Uint8(full, i)
	}
	return n
}

// Uints8 adds the field key with i as a []uint8 to the namespaced event.
func (n *NamespacedEvent) Uints8(key string, i []uint8) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Uints8(full, i)
	}
	return n
}

// Uints8V adds the field key with i as a []uint8 to the namespaced event.
func (n *NamespacedEvent) Uints8V(key string, i ...uint8) *NamespacedEvent {
	return n.Uints8(key, i)
}

// Uint16 adds the field key with i as a uint16 to the namespaced event.
func (n *NamespacedEvent) Uint16(key string, i uint16) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Uint16(full, i)
	}
	return n
}

// Uints16 adds the field key with i as a []uint16 to the namespaced event.
func (n *NamespacedEvent) Uints16(key string, i []uint16) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Uints16(full, i)
	}
	return n
}

// Uints16V adds the field key with i as a []uint16 to the namespaced event.
func (n *NamespacedEvent) Uints16V(key string, i ...uint16) *NamespacedEvent {
	return n.Uints16(key, i)
}

// Uint32 adds the field key with i as a uint32 to the namespaced event.
func (n *NamespacedEvent) Uint32(key string, i uint32) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Uint32(full, i)
	}
	return n
}

// Uints32 adds the field key with i as a []uint32 to the namespaced event.
func (n *NamespacedEvent) Uints32(key string, i []uint32) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Uints32(full, i)
	}
	return n
}

// Uints32V adds the field key with i as a []uint32 to the namespaced event.
func (n *NamespacedEvent) Uints32V(key string, i ...uint32) *NamespacedEvent {
	return n.Uints32(key, i)
}

// Uint64 adds the field key with i as a uint64 to the namespaced event.
func (n *NamespacedEvent) Uint64(key string, i uint64) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Uint64(full, i)
	}
	return n
}

// Uints64 adds the field key with i as a []uint64 to the namespaced event.
func (n *NamespacedEvent) Uints64(key string, i []uint64) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Uints64(full, i)
	}
	return n
}

// Uints64V adds the field key with i as a []uint64 to the namespaced event.
func (n *NamespacedEvent) Uints64V(key string, i ...uint64) *NamespacedEvent {
	return n.Uints64(key, i)
}

// Float32 adds the field key with f as a float32 to the namespaced event.
func (n *NamespacedEvent) Float32(key string, f float32) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Float32(full, f)
	}
	return n
}

// Floats32 adds the field key with f as a []float32 to the namespaced event.
func (n *NamespacedEvent) Floats32(key string, f []float32) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Floats32(full, f)
	}
	return n
}

// Floats32V adds the field key with f as a []float32 to the namespaced event.
func (n *NamespacedEvent) Floats32V(key string, f ...float32) *NamespacedEvent {
	return n.Floats32(key, f)
}

// Float64 adds the field key with f as a float64 to the namespaced event.
func (n *NamespacedEvent) Float64(key string, f float64) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Float64(full, f)
	}
	return n
}

// Floats64 adds the field key with f as a []float64 to the namespaced event.
func (n *NamespacedEvent) Floats64(key string, f []float64) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Floats64(full, f)
	}
	return n
}

// Floats64V adds the field key with f as a []float64 to the namespaced event.
func (n *NamespacedEvent) Floats64V(key string, f ...float64) *NamespacedEvent {
	return n.Floats64(key, f)
}

// Time adds the field key with t formatted as a string to the namespaced event.
func (n *NamespacedEvent) Time(key string, t time.Time) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Time(full, t)
	}
	return n
}

// Times adds the field key with t formatted as a string to the namespaced event.
func (n *NamespacedEvent) Times(key string, t []time.Time) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Times(full, t)
	}
	return n
}

// Dur adds the field key with duration d to the namespaced event.
func (n *NamespacedEvent) Dur(key string, d time.Duration) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Dur(full, d)
	}
	return n
}

// Durs adds the field key with durations d to the namespaced event.
func (n *NamespacedEvent) Durs(key string, d []time.Duration) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Durs(full, d)
	}
	return n
}

// TimeDiff adds the field key with positive duration between time t and start
// to the namespaced event.
func (n *NamespacedEvent) TimeDiff(key string, t time.Time, start time.Time) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.TimeDiff(full, t, start)
	}
	return n
}

// Any is a wrapper around Interface.
func (n *NamespacedEvent) Any(key string, i interface{}) *NamespacedEvent {
	return n.Interface(key, i)
}

// Interface adds the field key with i marshaled using reflection to the
// namespaced event.
func (n *NamespacedEvent) Interface(key string, i interface{}) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Interface(full, i)
	}
	return n
}

// Type adds the field key with val's type using reflection to the namespaced
// event.
func (n *NamespacedEvent) Type(key string, val interface{}) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Type(full, val)
	}
	return n
}

// IPAddr adds the field key with ip as a net.IP to the namespaced event.
func (n *NamespacedEvent) IPAddr(key string, ip net.IP) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.IPAddr(full, ip)
	}
	return n
}

// IPAddrs adds the field key with ip as a []net.IP to the namespaced event.
func (n *NamespacedEvent) IPAddrs(key string, ip []net.IP) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.IPAddrs(full, ip)
	}
	return n
}

// IPPrefix adds the field key with pfx as a net.IPNet to the namespaced event.
func (n *NamespacedEvent) IPPrefix(key string, pfx net.IPNet) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.IPPrefix(full, pfx)
	}
	return n
}

// IPPrefixes adds the field key with pfx as a []net.IPNet to the namespaced
// event.
func (n *NamespacedEvent) IPPrefixes(key string, pfx []net.IPNet) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.IPPrefixes(full, pfx)
	}
	return n
}

// MACAddr adds the field key with ha as a net.HardwareAddr to the namespaced
// event.
func (n *NamespacedEvent) MACAddr(key string, ha net.HardwareAddr) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.MACAddr(full, ha)
	}
	return n
}

// Object adds the field key with obj to the namespaced event. If obj
// implements NamespacedLogObjectMarshaler, its fields are marshaled with the
// full namespace path (e.g. "db.stats.rows"). Otherwise, obj is marshaled
// as a nested JSON object under the namespaced key, keeping its own namespace.
func (n *NamespacedEvent) Object(key string, obj LogObjectMarshaler) *NamespacedEvent {
	if full := n.check(key); full == "" {
		return n
	} else if obj == nil {
		n.e.Object(full, nil)
		return n
	} else if nom, ok := obj.(NamespacedLogObjectMarshaler); ok {
		sub := n.sub(key)
		nom.MarshalZerologNamespacedObject(sub)
		return n
	} else {
		n.e.Object(full, obj)
		return n
	}
}

// Objects adds the field key with objs as an array of objects to the
// namespaced event.
func (n *NamespacedEvent) Objects(key string, objs []LogObjectMarshaler) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Objects(full, objs)
	}
	return n
}

// ObjectsV adds the field key with objs as an array of objects to the
// namespaced event.
func (n *NamespacedEvent) ObjectsV(key string, objs ...LogObjectMarshaler) *NamespacedEvent {
	return n.Objects(key, objs)
}

// Array adds the field key with an array to the namespaced event.
func (n *NamespacedEvent) Array(key string, arr LogArrayMarshaler) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Array(full, arr)
	}
	return n
}

// Dict adds the field key with a dict to the namespaced event.
func (n *NamespacedEvent) Dict(key string, dict *Event) *NamespacedEvent {
	if full := n.check(key); full != "" {
		n.e.Dict(full, dict)
	}
	return n
}

// Fields is a helper function to use a map or slice to set fields using type
// assertion. Only map[string]interface{} and []interface{} are accepted.
// Keys are prefixed with the namespace.
func (n *NamespacedEvent) Fields(fields interface{}) *NamespacedEvent {
	if n == nil || !n.e.Enabled() || n.err != nil {
		return n
	}
	switch fields := fields.(type) {
	case []interface{}:
		if nn := len(fields); nn&0x1 == 1 { // odd number
			fields = fields[:nn-1]
		}
		for i := 0; i < len(fields); i += 2 {
			if key, ok := fields[i].(string); ok {
				n.Interface(key, fields[i+1])
			}
		}
	case map[string]interface{}:
		for key, val := range fields {
			n.Interface(key, val)
		}
	}
	return n
}

// Stack enables stack trace printing for the error passed to Err().
func (n *NamespacedEvent) Stack() *NamespacedEvent {
	if n != nil && n.e != nil {
		n.e.Stack()
	}
	return n
}

// Ctx adds the Go Context to the event context. The context is not rendered
// in the output message, but is available to hooks and to Func() calls via the
// GetCtx() accessor.
func (n *NamespacedEvent) Ctx(ctx context.Context) *NamespacedEvent {
	if n != nil && n.e != nil {
		n.e.Ctx(ctx)
	}
	return n
}

// GetCtx retrieves the Go context.Context which is optionally stored in the
// event.
func (n *NamespacedEvent) GetCtx() context.Context {
	if n == nil || n.e == nil {
		return context.Background()
	}
	return n.e.GetCtx()
}

// CallerSkipFrame instructs any future Caller calls to skip the specified
// number of frames.
func (n *NamespacedEvent) CallerSkipFrame(skip int) *NamespacedEvent {
	if n != nil && n.e != nil {
		n.e.CallerSkipFrame(skip)
	}
	return n
}
