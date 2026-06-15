// Package discovery finds DLNA MediaRenderer devices on the LAN via SSDP and
// exposes typed AVTransport/RenderingControl clients for each.
package discovery

import (
	"context"
	"fmt"
	"net/url"

	"github.com/huin/goupnp/dcps/av1"
)

// Device is a discovered DLNA renderer with the service clients we control it through.
type Device struct {
	FriendlyName string
	Location     *url.URL // device description URL; host changes on TV reboot
	AVTransport  *av1.AVTransport1
	// Rendering may be nil if the device exposes no RenderingControl service.
	Rendering *av1.RenderingControl1
}

// Discover performs an SSDP search and returns every renderer that exposes an
// AVTransport service. RenderingControl is matched in by device location.
func Discover(ctx context.Context) ([]Device, error) {
	avClients, _, err := av1.NewAVTransport1ClientsCtx(ctx)
	if err != nil {
		return nil, fmt.Errorf("ssdp search for AVTransport: %w", err)
	}

	rcClients, _, _ := av1.NewRenderingControl1ClientsCtx(ctx)
	rcByLocation := make(map[string]*av1.RenderingControl1, len(rcClients))
	for _, rc := range rcClients {
		if rc.Location != nil {
			rcByLocation[rc.Location.String()] = rc
		}
	}

	devices := make([]Device, 0, len(avClients))
	for _, av := range avClients {
		d := Device{
			FriendlyName: "unknown",
			Location:     av.Location,
			AVTransport:  av,
		}
		if av.RootDevice != nil {
			if name := av.RootDevice.Device.FriendlyName; name != "" {
				d.FriendlyName = name
			}
		}
		if av.Location != nil {
			d.Rendering = rcByLocation[av.Location.String()]
		}
		devices = append(devices, d)
	}
	return devices, nil
}

// FindByURL builds a Device from a known device-description or control URL,
// skipping SSDP. Used when the user passes -t with a saved URL.
func FindByURL(ctx context.Context, loc *url.URL) (*Device, error) {
	avClients, err := av1.NewAVTransport1ClientsByURLCtx(ctx, loc)
	if err != nil {
		return nil, fmt.Errorf("load AVTransport from %s: %w", loc, err)
	}
	if len(avClients) == 0 {
		return nil, fmt.Errorf("no AVTransport service at %s", loc)
	}
	av := avClients[0]
	d := &Device{FriendlyName: "unknown", Location: av.Location, AVTransport: av}
	if av.RootDevice != nil && av.RootDevice.Device.FriendlyName != "" {
		d.FriendlyName = av.RootDevice.Device.FriendlyName
	}
	if rcClients, err := av1.NewRenderingControl1ClientsByURLCtx(ctx, loc); err == nil && len(rcClients) > 0 {
		d.Rendering = rcClients[0]
	}
	return d, nil
}
