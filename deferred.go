package zerolog

import "fmt"

// FailureMode controls what happens when a deferred field generator fails.
type FailureMode int

const (
	// RejectOnFailure discards the entire event when any generator fails.
	// Nothing is written to the output. This is the default failure mode.
	RejectOnFailure FailureMode = iota

	// MarkerOnFailure writes a fixed failure marker instead of the failed
	// fields. The marker is a constant and never contains the generator's
	// error text.
	MarkerOnFailure
)

// deferredField is a single registered field paired with its generator.
type deferredField struct {
	key string
	gen func() (interface{}, error)
}

// DeferredEvent is an event wrapper that defers field generation until the
// event is committed. Fields are registered with Add and generators run in
// registration order only when the event is enabled. If any generator fails,
// the event is either rejected (nothing written) or marked with a fixed
// failure marker that never contains the error text.
//
// The normal Event path is unchanged; this is an opt-in layer.
type DeferredEvent struct {
	e      *Event
	fields []deferredField
	mode   FailureMode
	done   bool
}

// Deferred begins a deferred field registration layer for this event.
// Generators registered via Add are not executed until Msg or Send is
// called, and only if the event is enabled (level not filtered and not
// sampled out).
func (e *Event) Deferred() *DeferredEvent {
	d := &DeferredEvent{}
	if e != nil {
		d.e = e
	}
	return d
}

// OnFailure sets the failure mode. The default is RejectOnFailure.
func (d *DeferredEvent) OnFailure(mode FailureMode) *DeferredEvent {
	if d != nil {
		d.mode = mode
	}
	return d
}

// Add registers a deferred field. The generator is not called until the
// event is committed, and only if the event is enabled. Generators run in
// the order they are registered. Each generator runs at most once.
func (d *DeferredEvent) Add(key string, gen func() (interface{}, error)) *DeferredEvent {
	if d != nil {
		d.fields = append(d.fields, deferredField{key: key, gen: gen})
	}
	return d
}

// Msg commits the event with msg added as the message field if not empty.
// Generators run in registration order only if the event is enabled. If any
// generator fails, the event is rejected or marked depending on the failure
// mode.
func (d *DeferredEvent) Msg(msg string) {
	if d == nil || d.done {
		return
	}
	d.done = true
	e := d.e
	if e == nil || !e.Enabled() {
		// Event filtered by level or sampling: generators must not run.
		if e != nil {
			putEvent(e)
		}
		return
	}
	if len(d.fields) == 0 {
		e.msg(msg)
		return
	}
	tmp, ok := d.buildFields()
	if !ok {
		d.fail(e, msg)
		return
	}
	// All generators succeeded: commit the fields to the event buffer and
	// proceed with the normal write path.
	e.buf = enc.AppendObjectData(e.buf, tmp)
	e.msg(msg)
}

// Send is equivalent to calling Msg("").
func (d *DeferredEvent) Send() {
	d.Msg("")
}

// Msgf sends the event with a formatted message.
func (d *DeferredEvent) Msgf(format string, v ...interface{}) {
	d.Msg(fmt.Sprintf(format, v...))
}

// buildFields runs all generators in registration order and builds the
// field content. It returns false if any generator fails. The field content
// is built in a temporary buffer and is not committed to the event until
// every generator succeeds, so a failure never produces half a JSON record.
func (d *DeferredEvent) buildFields() ([]byte, bool) {
	tmp := enc.AppendBeginMarker(make([]byte, 0, 64))
	for _, f := range d.fields {
		v, err := f.gen()
		if err != nil {
			return nil, false
		}
		tmp = enc.AppendKey(tmp, f.key)
		tmp = enc.AppendInterface(tmp, v)
	}
	return tmp, true
}

// fail handles a generator failure according to the failure mode.
func (d *DeferredEvent) fail(e *Event, msg string) {
	switch d.mode {
	case MarkerOnFailure:
		// Write a fixed failure marker. The marker is a constant and never
		// contains the generator's error text.
		tmp := enc.AppendBeginMarker(make([]byte, 0, 32))
		tmp = enc.AppendKey(tmp, deferredFailureMarkerKey)
		tmp = enc.AppendBool(tmp, deferredFailureMarkerValue)
		e.buf = enc.AppendObjectData(e.buf, tmp)
		e.msg(msg)
	default:
		// Reject: discard the entire event. Nothing is written.
		putEvent(e)
	}
}

const (
	deferredFailureMarkerKey   = "deferred_failed"
	deferredFailureMarkerValue = true
)
