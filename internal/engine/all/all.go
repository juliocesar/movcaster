// Package all registers every concrete engine. main imports it for its side
// effects; adding a TV family is a new package plus one line here.
//
// Registration order is preference order: when one TV answers on several
// engines (a Google TV also runs a generic DLNA renderer), discovery lists it
// once, under the engine registered first, and a bare -t IP tries engines in
// this order. Register a TV family's native protocol ahead of DLNA, which many
// TVs expose as a lowest-common-denominator extra. -t dlna:<ip> or
// --engine dlna still reaches a secondary engine explicitly.
package all

import (
	"github.com/juliocesar/movcaster/internal/engine"
	"github.com/juliocesar/movcaster/internal/engine/dlna"
	"github.com/juliocesar/movcaster/internal/engine/googlecast"
)

func init() {
	engine.Register(googlecast.New())
	engine.Register(dlna.New())
}
