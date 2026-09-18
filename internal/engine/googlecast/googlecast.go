// Package googlecast is the Google Cast engine: mDNS discovery plus the CASTV2
// protocol (protobuf-framed JSON over TLS on port 8009) driving the Default
// Media Receiver. Built for Chromecast-built-in TVs (Google TV panels).
package googlecast

import (
	"context"
	"net"

	"github.com/juliocesar/movcaster/internal/engine"
)

// New returns the Google Cast engine.
func New() engine.Engine { return castEngine{} }

type castEngine struct{}

func (castEngine) Name() string                   { return "googlecast" }
func (castEngine) DefaultProfile() engine.Profile { return defaultReceiverProfile }

// Discover browses mDNS for _googlecast._tcp devices (see discover.go).
func (castEngine) Discover(ctx context.Context) ([]engine.Device, error) {
	return discover(ctx)
}

// Resolve dials the target's Cast port directly and performs a
// CONNECT + GET_STATUS. If the device answers, it is a Cast device; a refused
// connection or TLS failure means it is not ours (ErrNotThisEngine). This makes
// `-t <ip>` work without any mDNS at all — the escape hatch for macOS mDNS
// flakiness.
func (castEngine) Resolve(ctx context.Context, target string) (engine.Device, error) {
	host, port := target, 8009
	if h, p, err := net.SplitHostPort(target); err == nil {
		host = h
		if n, perr := net.LookupPort("tcp", p); perr == nil {
			port = n
		}
	}
	if host == "" || net.ParseIP(host) == nil && !plausibleHostname(host) {
		return nil, engine.ErrNotThisEngine
	}
	c, err := dial(ctx, net.JoinHostPort(host, itoa(port)))
	if err != nil {
		return nil, engine.ErrNotThisEngine
	}
	defer c.close(nil)
	if err := c.connectPlatform(); err != nil {
		return nil, engine.ErrNotThisEngine
	}
	if _, err := c.getReceiverStatus(ctx); err != nil {
		return nil, engine.ErrNotThisEngine
	}
	return &device{name: host, host: host, port: port}, nil
}

// plausibleHostname rejects targets that are clearly not a host (URLs with
// schemes land here as "http" after a SplitHostPort miss).
func plausibleHostname(s string) bool {
	for _, r := range s {
		if r == '/' || r == ' ' {
			return false
		}
	}
	return s != "" && s != "http" && s != "https"
}

// device is one discovered (or directly resolved) Cast device.
type device struct {
	name  string // fn= TXT record, falling back to the instance name or host
	host  string // resolved IPv4
	port  int    // SRV port, normally 8009
	id    string // id= TXT record: a stable device UUID (future per-device config)
	model string // md= TXT record, e.g. "Chromecast" / a TV model
}

func (d *device) Name() string { return d.name }
func (d *device) Host() string { return d.host }
func (d *device) Addr() string {
	return net.JoinHostPort(d.host, itoa(d.port))
}
func (d *device) Engine() string          { return "googlecast" }
func (d *device) Profile() engine.Profile { return defaultReceiverProfile }

// Open dials the device and performs the app handshake. The caller Closes it.
func (d *device) Open(ctx context.Context) (engine.Session, error) {
	return newSession(ctx, d.Addr())
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
