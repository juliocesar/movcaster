package googlecast

import (
	"context"
	"testing"
	"time"

	"github.com/juliocesar/movcaster/internal/engine/enginetest"
)

// The full conformance suite runs against the in-process fake receiver: the
// device is obtained the way a user's -t <ip> would be, via Resolve.
func TestConformance(t *testing.T) {
	f := newFakeReceiver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dev, err := New().Resolve(ctx, f.addr())
	if err != nil {
		t.Fatal(err)
	}
	enginetest.Run(t, New(), dev)
}
