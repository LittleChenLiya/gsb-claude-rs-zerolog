# Component namespaces (`Event.Namespace`)

When several components contribute fields to the same log line they must not
overwrite each other's business keys, and they must never clobber the core
reserved fields. `Event.Namespace` is an **explicitly enabled** wrapper
around `*Event` that enforces this.

The normal `*Event` hot path is untouched: the wrapper is allocated only
when `Namespace(...)` is called, and a filtered / sampled-out event returns
a cheap disabled wrapper whose field methods are no-ops (so expensive field
construction is skipped, exactly like `Event.Enabled` / `Event.Func`).

## API

```go
ns := logger.Info().Namespace(zerolog.NamespaceDupReject)
defer ns.Msg("done")

ns.Component("db").Str("id", "42").Int("rows", 3)
ns.Component("http").Str("id", "req-7").Int("status", 200)
```

- `(*Event).Namespace(policy NamespaceDupPolicy) *NamespaceEvent` — opt in.
- `Component(name string)` — open a component block; later fields are emitted
  under that component. Fields added before the first `Component` call are
  attached at the top level (they still obey the rules below).
- Duplicate policy:
  - `NamespaceDupReject` (default zero value): a second write to the same
    key **inside one component** collects an error; at submit time the whole
    event is refused.
  - `NamespaceDupKeepFirst`: a second write to the same key inside one
    component is silently dropped; only the first value is kept.
  - The same key in **different** components is never a duplicate.
- `Msg` / `Msgf` / `Send` validate the whole event and then emit it exactly
  once. `Err()` returns the validation errors collected so far. `Func(func)`
  runs only while the wrapped event is enabled.

Scalar/container helpers mirror `*Event`: `Str, Bytes, Bool, Int, Int64,
Uint64, Float64, Time, Dur, Interface/Any, Stringer, IPAddr, AnErr, Strs,
Ints, Array, Object`.

## Rules

1. **Isolation** — each component is emitted as its own object, so the
   dotted identities `db.id` and `http.id` never collide.
2. **Reserved fields** — components may not write `time`, `level`,
   `message`, `error`, `stack` or `caller` (the live values of the
   configurable `*FieldName` globals). The attempt is rejected; only the
   logger core sets them, at the top level.
3. **Nested objects keep their own namespace** — an `Object`/`Array`
   written by a component lives inside that component object; its inner
   keys cannot collide with siblings or with another component.
4. **Deferred, all-or-nothing validation** — field writes only collect
   errors; nothing is checked against the writer until `Msg`/`Send`. On any
   validation failure the buffered fields are discarded and the underlying
   writer is **never** called, so no half log line is emitted. Validation
   errors go through `zerolog.ErrorHandler` (falling back to stderr).
5. **Sampling / disabled levels are preserved** — the wrapper shares the
   parent logger's decision; a disabled event yields a disabled wrapper and
   runs no field constructors.

## Output protocol

One JSON object per line (`application/x-ndjson`), unchanged from zerolog.
Core reserved fields stay top-level; each component contributes one nested
object whose key is the component name:

```
{"level":"<level>","message":"<msg>","time":<optional>,"<component>":{"<business-key>":<value>, ...}, ...}
```

Components appear in first-use order; fields within a component appear in
write order. With CBOR (`-tags binary_log`) the same structure is encoded as
one CBOR map per line; the rules are identical.

## Short log examples

Two components writing `id` (`db.id` vs `http.id`) plus a nested object:

```json
{"level":"info","db":{"id":"42","rows":3,"pool":{"primary":"db-primary","replica":"db-replica"}},"http":{"id":"req-7","status":200},"message":"request handled"}
```

A component trying to overwrite a reserved field produces **no log line**;
the validation error is reported instead:

```
zerolog: namespace "db": field "level" rejected: reserved field name, only the logger core may set it
```

Same-component duplicate with `NamespaceDupKeepFirst` — only the first value
is kept and the event is still emitted:

```json
{"level":"info","db":{"id":"first"},"message":"keep first value"}
```

A below-level (`Debug` on an `Info` logger) or sampled-out event emits
nothing and never runs the expensive constructor:

```go
if e := log.Debug().Namespace(zerolog.NamespaceDupReject); e.Enabled() {
    e.Component("db").Str("id", expensiveID()).Msg("never emitted")
}
```

Run the samples with:

```sh
go run ./cmd/nsdemo
```

