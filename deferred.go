package zerolog

// DeferredFieldGenerator lazily produces the value of a log field.
//
// A generator registered with Event.Defer is invoked at most once, and
// only when the event is actually enabled: events filtered out by level
// or sampling never trigger their generators (zero invocations).
// Generators are evaluated in registration order.
type DeferredFieldGenerator func() (interface{}, error)

// DeferFailurePolicy selects what Event.MsgDeferred does when a deferred
// field generator returns an error.
type DeferFailurePolicy int

const (
	// DeferReject drops the whole event when any deferred generator
	// fails. Nothing is written to the underlying writer, so the writer
	// never observes a partially encoded record. This is the default
	// policy.
	DeferReject DeferFailurePolicy = iota

	// DeferMark commits the event even when a deferred generator fails.
	// The failed field is written with the fixed DeferredFailureMarker
	// value, which never contains the original error text. Remaining
	// generators are still evaluated, each at most once.
	DeferMark
)

// DeferredFailureMarker is the fixed placeholder written in place of a
// deferred field whose generator failed when the DeferMark policy is in
// effect. It deliberately carries no information about the underlying
// error, so sensitive failure details cannot leak into log output.
const DeferredFailureMarker = "<deferred-field-failed>"

// deferredField is a field registered for lazy evaluation at commit time.
type deferredField struct {
	key string
	gen DeferredFieldGenerator
}

// Defer registers a field whose value is produced lazily by gen. The
// generator runs only when the event is committed with MsgDeferred and
// only if the event is enabled (not filtered by level or sampling).
// Generators run in registration order and each runs at most once.
//
// Defer is a no-op on a nil (disabled or sampled-out) event, so it can be
// chained exactly like the regular field methods. The normal Event path
// (Str/Int/.../Msg) is unchanged by this layer.
func (e *Event) Defer(key string, gen DeferredFieldGenerator) *Event {
	if e == nil {
		return e
	}
	e.deferred = append(e.deferred, deferredField{key: key, gen: gen})
	return e
}

// DeferOnFailure sets the policy applied by MsgDeferred when a deferred
// generator fails. The default is DeferReject.
func (e *Event) DeferOnFailure(policy DeferFailurePolicy) *Event {
	if e == nil {
		return e
	}
	e.deferPolicy = policy
	return e
}

// MsgDeferred evaluates the deferred fields registered with Defer, in
// registration order, and commits the event.
//
// All generator results are collected before anything is encoded, so the
// event is committed only when every generator succeeded:
//
//   - DeferReject (default): if any generator fails, the event is dropped
//     and the writer never sees a partial record.
//   - DeferMark: failed fields are written with the fixed
//     DeferredFailureMarker value (the original error text is never
//     written) and the event is committed.
//
// Events disabled by level or sampling never reach this point: their
// generators are invoked zero times. Each generator is invoked at most
// once, even if MsgDeferred is called again on the same event.
//
// NOTICE: once this method is called, the *Event should be disposed.
func (e *Event) MsgDeferred(msg string) {
	if e == nil {
		return
	}
	if len(e.deferred) == 0 || e.level == Disabled {
		// No deferred work: identical to the regular Msg path.
		e.msg(msg)
		return
	}
	type kv struct {
		key string
		val interface{}
	}
	vals := make([]kv, 0, len(e.deferred))
	for _, d := range e.deferred {
		v, err := d.gen()
		if err != nil {
			if e.deferPolicy == DeferReject {
				// Drop the event. Nothing was appended to the buffer
				// for the deferred fields and nothing was written to
				// the writer, so no partial record can leak.
				putEvent(e)
				return
			}
			v = DeferredFailureMarker
		}
		vals = append(vals, kv{key: d.key, val: v})
	}
	// All generators succeeded (or failures were marked): encode the
	// fields in registration order and commit the complete record.
	for _, f := range vals {
		e.Interface(f.key, f.val)
	}
	e.deferred = nil
	e.msg(msg)
}
