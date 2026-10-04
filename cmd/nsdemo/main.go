// Command nsdemo prints short log lines that exercise the explicit
// component-namespace Event wrapper: per-component fields, reserved-field
// protection, the two duplicate policies, nested objects and a disabled
// (filtered) event whose expensive fields are never built.
package main

import (
	"os"

	"github.com/rs/zerolog"
)

type connPool struct{}

func (connPool) MarshalZerologObject(e *zerolog.Event) {
	e.Str("primary", "db-primary").Str("replica", "db-replica")
}

func expensiveID() string {
	panic("expensive field constructor must not run for a disabled event")
}

func main() {
	log := zerolog.New(os.Stdout).Level(zerolog.InfoLevel)

	// 1. Two components write the same business key "id": db.id / http.id.
	log.Info().Namespace(zerolog.NamespaceDupReject).
		Component("db").Str("id", "42").Int("rows", 3).
		Object("pool", connPool{}).
		Component("http").Str("id", "req-7").Int("status", 200).
		Msg("request handled")

	// 2. A component tries to overwrite a reserved field: rejected, and
	//    because validation runs at submit time, nothing is written.
	log.Info().Namespace(zerolog.NamespaceDupReject).
		Component("db").Str("id", "9").Str("level", "sneaky").
		Msg("this line is refused")

	// 3. Same-namespace duplicate with keep-first: only the first value
	//    survives and the event is emitted.
	log.Info().Namespace(zerolog.NamespaceDupKeepFirst).
		Component("db").Str("id", "first").Str("id", "second").
		Msg("keep first value")

	// 4. A disabled (below-level) event: the expensive constructor is
	//    skipped, no line is produced.
	if e := log.Debug().Namespace(zerolog.NamespaceDupReject); e.Enabled() {
		e.Component("db").Str("id", expensiveID()).Msg("never emitted")
	}
}
