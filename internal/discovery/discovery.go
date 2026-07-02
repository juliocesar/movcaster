// Package discovery finds DLNA MediaRenderer devices on the LAN via SSDP and
// exposes typed AVTransport/RenderingControl clients for each.
package discovery

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/av1"
	"github.com/huin/goupnp/ssdp"
)

// Device is a discovered DLNA renderer with the service clients we control it through.
type Device struct {
	FriendlyName string
	Location     *url.URL // device description URL; host changes on TV reboot
	AVTransport  *av1.AVTransport1
	// Rendering may be nil if the device exposes no RenderingControl service.
	Rendering *av1.RenderingControl1
}

// searchTargets are the SSDP search targets probed by Discover. A just-woke
// webOS TV routinely answers one of these while missing another (its DLNA stack
// comes up a beat after the panel), so we query several and merge. The old code
// searched only the exact AVTransport service URN, which silently lost any
// renderer that happened to answer its device URN or ssdp:all first.
var searchTargets = []string{
	av1.URN_AVTransport_1,
	"urn:schemas-upnp-org:device:MediaRenderer:1",
	ssdp.SSDPAll,
}

// Discover performs an SSDP search and returns every renderer that exposes an
// AVTransport service. It probes several search targets concurrently and merges
// the results (deduped by description URL), so a renderer that misses one
// M-SEARCH is still found via another.
func Discover(ctx context.Context) ([]Device, error) {
	roots, err := discoverRoots(ctx)
	if err != nil {
		return nil, err
	}
	devices := make([]Device, 0, len(roots))
	for _, r := range roots {
		if d, ok := deviceFromRoot(r.Root, r.Location); ok {
			devices = append(devices, d)
		}
	}
	return devices, nil
}

// discoverRoots runs each search target concurrently and returns the unique,
// cleanly-probed root devices. Every target caps its own wait inside goupnp
// (~2s), so the concurrent set finishes in about that time rather than the sum.
func discoverRoots(ctx context.Context) ([]goupnp.MaybeRootDevice, error) {
	var (
		mu   sync.Mutex
		all  []goupnp.MaybeRootDevice
		errs []error
		wg   sync.WaitGroup
	)
	for _, st := range searchTargets {
		wg.Add(1)
		go func(st string) {
			defer wg.Done()
			found, err := goupnp.DiscoverDevicesCtx(ctx, st)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			all = append(all, found...)
		}(st)
	}
	wg.Wait()

	seen := make(map[string]bool)
	uniq := make([]goupnp.MaybeRootDevice, 0, len(all))
	for _, m := range all {
		if m.Err != nil || m.Root == nil || m.Location == nil {
			continue
		}
		key := m.Location.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		uniq = append(uniq, m)
	}
	// Only surface a send error when every target failed and nothing turned up;
	// a partial failure that still found a renderer is not worth reporting.
	if len(uniq) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("ssdp search: %w", errs[0])
	}
	return uniq, nil
}

// deviceFromRoot builds a Device from an already-discovered root device without
// any further SSDP. It returns ok=false for roots that expose no AVTransport
// (e.g. a TV's non-renderer UPnP descriptions, which ssdp:all also surfaces).
func deviceFromRoot(root *goupnp.RootDevice, loc *url.URL) (Device, bool) {
	avs, err := av1.NewAVTransport1ClientsFromRootDevice(root, loc)
	if err != nil || len(avs) == 0 {
		return Device{}, false
	}
	av := avs[0]
	d := Device{FriendlyName: "unknown", Location: av.Location, AVTransport: av}
	if av.RootDevice != nil && av.RootDevice.Device.FriendlyName != "" {
		d.FriendlyName = av.RootDevice.Device.FriendlyName
	}
	if rcs, err := av1.NewRenderingControl1ClientsFromRootDevice(root, loc); err == nil && len(rcs) > 0 {
		d.Rendering = rcs[0]
	}
	return d, true
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

// FindByHost resolves a renderer by its host/IP with a targeted SSDP M-SEARCH,
// then loads the advertised description. It sends to the multicast group (webOS
// ignores unicast M-SEARCH) but keeps only replies from the wanted host, and
// listens on its own longer, any-ST window rather than goupnp's capped 2s exact
// match. That learns the TV's dynamic control port and recovers a specific TV
// that a broad Discover keeps missing, so `-t <ip>` and resuming a saved TV
// keep working when the general search comes up empty.
func FindByHost(ctx context.Context, host string) (*Device, error) {
	locs, err := searchHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(locs) == 0 {
		return nil, fmt.Errorf("no SSDP response from %s", host)
	}
	var firstErr error
	for _, loc := range locs {
		if d, err := FindByURL(ctx, loc); err == nil {
			return d, nil
		} else if firstErr == nil {
			firstErr = err
		}
	}
	return nil, fmt.Errorf("no AVTransport renderer at %s: %w", host, firstErr)
}

const ssdpMulticast = "239.255.255.250:1900"

// searchHost sends an M-SEARCH to the SSDP multicast group and returns the
// LOCATION URLs advertised by the wanted host only, ordered so renderer/transport
// responses come first (a webOS TV publishes several unrelated UPnP descriptions;
// only one carries AVTransport).
func searchHost(ctx context.Context, host string) ([]*url.URL, error) {
	want := net.ParseIP(host)
	if want == nil { // host may be a name; resolve to compare reply source IPs
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
		want = ips[0]
	}
	group, err := net.ResolveUDPAddr("udp4", ssdpMulticast)
	if err != nil {
		return nil, err
	}
	// Bind to the local address that routes to the host, so the multicast query
	// leaves the right interface and replies come back to us.
	laddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	if c, err := net.Dial("udp4", net.JoinHostPort(host, "1900")); err == nil {
		if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
			laddr = &net.UDPAddr{IP: ua.IP, Port: 0}
		}
		c.Close()
	}
	conn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return nil, fmt.Errorf("open udp socket: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	for _, st := range []string{"urn:schemas-upnp-org:device:MediaRenderer:1", ssdp.SSDPAll} {
		msg := "M-SEARCH * HTTP/1.1\r\n" +
			"HOST: " + ssdpMulticast + "\r\n" +
			"MAN: \"ssdp:discover\"\r\n" +
			"MX: 2\r\n" +
			"ST: " + st + "\r\n\r\n"
		if _, err := conn.WriteToUDP([]byte(msg), group); err != nil {
			return nil, fmt.Errorf("send M-SEARCH for %s: %w", host, err)
		}
	}

	var order []*url.URL
	seen := make(map[string]bool)
	priority := make(map[string]bool)
	buf := make([]byte, 2048)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // deadline reached or socket closed
		}
		if !src.IP.Equal(want) {
			continue // ignore anything not from the target host
		}
		resp, perr := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if perr != nil {
			continue
		}
		loc := resp.Header.Get("Location")
		st := resp.Header.Get("St")
		resp.Body.Close()
		u, perr := url.Parse(loc)
		if perr != nil || u.Host == "" {
			continue
		}
		key := u.String()
		if strings.Contains(st, "MediaRenderer") || strings.Contains(st, "AVTransport") {
			priority[key] = true
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		order = append(order, u)
	}
	sort.SliceStable(order, func(i, j int) bool {
		return priority[order[i].String()] && !priority[order[j].String()]
	})
	return order, nil
}
