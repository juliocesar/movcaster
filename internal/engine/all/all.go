// Package all registers every concrete engine. main imports it for its side
// effects; adding a TV family is a new package plus one line here.
package all

import (
	"github.com/juliocesar/movcaster/internal/engine"
	"github.com/juliocesar/movcaster/internal/engine/dlna"
)

func init() {
	engine.Register(dlna.New())
}
