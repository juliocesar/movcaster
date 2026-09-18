// Package dlna is the DLNA/UPnP engine: SSDP discovery plus AVTransport and
// RenderingControl SOAP control, grown against an LG webOS TV. Its quirks
// (LG_TRANSITIONING waits, 701 retries, the multicast-only host probe) live
// here, behind the neutral engine contract.
package dlna

import (
	"context"
	"net"
	"net/url"
	"strings"

	"github.com/huin/goupnp/dcps/av1"
	"github.com/juliocesar/movcaster/internal/engine"
)

// New returns the DLNA engine.
func New() engine.Engine { return dlnaEngine{} }

type dlnaEngine struct{}

func (dlnaEngine) Name() string                   { return "dlna" }
func (dlnaEngine) DefaultProfile() engine.Profile { return webOSProfile }

// Discover finds every AVTransport renderer on the LAN (see discover.go).
func (dlnaEngine) Discover(ctx context.Context) ([]engine.Device, error) {
	devs, err := discover(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]engine.Device, len(devs))
	for i := range devs {
		out[i] = devs[i]
	}
	return out, nil
}

// Resolve builds a Device from an explicit target without a broadcast search.
// An http(s) URL with a port loads the description directly; a bare host/IP is
// probed with the multicast-filtered M-SEARCH (webOS ignores unicast M-SEARCH
// and its control port is dynamic, so a URL cannot be guessed). No SSDP answer
// means the host is not a DLNA renderer as far as we can tell, so the registry
// gets ErrNotThisEngine and tries the next engine.
func (dlnaEngine) Resolve(ctx context.Context, target string) (engine.Device, error) {
	if u, err := url.Parse(ensureScheme(target)); err == nil && u.Host != "" && u.Port() != "" {
		if d, err := findByURL(ctx, u); err == nil {
			return d, nil
		}
	}
	host := hostOf(target)
	if host == "" {
		return nil, engine.ErrNotThisEngine
	}
	d, err := findByHost(ctx, host)
	if err != nil {
		return nil, engine.ErrNotThisEngine
	}
	return d, nil
}

// device is a discovered DLNA renderer. Holding it costs nothing: the SOAP
// clients are stateless, so Open does no I/O.
type device struct {
	friendlyName string
	location     *url.URL // device description URL; host changes on TV reboot
	av           *av1.AVTransport1
	// rc may be nil if the device exposes no RenderingControl service.
	rc *av1.RenderingControl1
}

func (d *device) Name() string { return d.friendlyName }
func (d *device) Host() string {
	if d.location == nil {
		return ""
	}
	return hostOf(d.location.Host)
}
func (d *device) Addr() string {
	if d.location == nil {
		return ""
	}
	return d.location.Host
}
func (d *device) Engine() string          { return "dlna" }
func (d *device) Profile() engine.Profile { return webOSProfile }

// HasVolumeControl reports whether the device exposes RenderingControl. It is
// an optional capability the CLI's -l listing sniffs for.
func (d *device) HasVolumeControl() bool { return d.rc != nil }

// Open returns the control session. DLNA is stateless SOAP, so this does no
// I/O and Close is a no-op. The nil check matters: assigning a nil *typed*
// pointer into the session's interface field would make HasVolume lie.
func (d *device) Open(context.Context) (engine.Session, error) {
	s := &session{av: d.av}
	if d.rc != nil {
		s.rc = d.rc
	}
	return s, nil
}

func ensureScheme(s string) string {
	if !strings.Contains(s, "://") {
		return "http://" + s
	}
	return s
}

// hostOf extracts the bare host/IP from an IP, IP:port, or URL.
func hostOf(s string) string {
	u, err := url.Parse(ensureScheme(s))
	if err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}
