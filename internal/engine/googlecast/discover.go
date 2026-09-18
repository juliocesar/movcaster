package googlecast

import (
	"context"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/juliocesar/movcaster/internal/engine"
)

// discover browses _googlecast._tcp via mDNS and returns every answering
// device. macOS caveat: a Go mDNS listener shares UDP 5353 with the system
// mDNSResponder, which needs SO_REUSEPORT plus a correct multicast join —
// zeroconf does both, but if discovery proves unreliable on a machine, the
// documented fallback is `-t <ip>` (Resolve needs no mDNS) or shelling out to
// `dns-sd`. Decide based on evidence against real hardware.
func discover(ctx context.Context) ([]engine.Device, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}

	// Bound the browse: engines are expected to return when ctx does, and the
	// caller's quick pass is 5s.
	bctx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		bctx, cancel = context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
	}

	entries := make(chan *zeroconf.ServiceEntry, 16)
	if err := resolver.Browse(bctx, "_googlecast._tcp", "local.", entries); err != nil {
		return nil, err
	}

	var devices []engine.Device
	seen := make(map[string]bool)
	for e := range entries { // zeroconf closes the channel when bctx ends
		d := deviceFromEntry(e)
		if d == nil || seen[d.host] {
			continue
		}
		seen[d.host] = true
		devices = append(devices, d)
	}
	return devices, nil
}

// deviceFromEntry builds a device from one mDNS answer. TXT records carry the
// useful identity: fn= is the user-visible name, id= a stable UUID, md= the
// model.
func deviceFromEntry(e *zeroconf.ServiceEntry) *device {
	if e == nil || len(e.AddrIPv4) == 0 {
		return nil
	}
	d := &device{
		name: strings.TrimSpace(e.Instance),
		host: e.AddrIPv4[0].String(),
		port: e.Port,
	}
	if d.port == 0 {
		d.port = 8009
	}
	for _, txt := range e.Text {
		k, v, ok := strings.Cut(txt, "=")
		if !ok {
			continue
		}
		switch k {
		case "fn":
			if v != "" {
				d.name = v
			}
		case "id":
			d.id = v
		case "md":
			d.model = v
		}
	}
	if d.name == "" {
		d.name = d.host
	}
	if d.model != "" {
		d.name += " (" + d.model + ")"
	}
	return d
}
