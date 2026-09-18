package dlna_test

import (
	"testing"

	"github.com/juliocesar/movcaster/internal/engine/dlna"
	"github.com/juliocesar/movcaster/internal/engine/enginetest"
)

// The DLNA engine has no in-process fake device (its transport is SOAP over a
// discovered description), so only the engine-level conformance runs here; the
// session behavior is covered by the fakeAV tests in this package.
func TestConformance(t *testing.T) {
	enginetest.Run(t, dlna.New(), nil)
}
