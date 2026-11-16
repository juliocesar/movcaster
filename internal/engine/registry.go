package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

var registry []Engine

// Register adds an engine. Call from an init() in internal/engine/all;
// registration order is the order engines are tried by ResolveTarget.
func Register(e Engine) { registry = append(registry, e) }

// Engines returns the registered engines in registration order.
func Engines() []Engine { return registry }

// Lookup returns the engine with the given Name.
func Lookup(name string) (Engine, bool) {
	for _, e := range registry {
		if e.Name() == name {
			return e, true
		}
	}
	return nil, false
}

// DiscoverAll runs every engine's Discover concurrently and merges the results.
// A single engine's failure is collected, never fatal: a broken mDNS stack must
// not stop DLNA discovery from working. only restricts to one engine ("" = all).
func DiscoverAll(ctx context.Context, only string) ([]Device, []error) {
	var (
		mu      sync.Mutex
		devices []Device
		errs    []error
		wg      sync.WaitGroup
	)
	for _, e := range registry {
		if only != "" && e.Name() != only {
			continue
		}
		wg.Add(1)
		go func(e Engine) {
			defer wg.Done()
			found, err := e.Discover(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
				return
			}
			devices = append(devices, found...)
		}(e)
	}
	wg.Wait()
	return devices, errs
}

// ResolveTarget parses a user target and returns the matching device.
// Forms:
//
//	"192.168.1.5"                try every engine in registration order
//	"192.168.1.5:8009"           same, with an explicit port
//	"googlecast:192.168.1.5"     restrict to one engine
//	"http://host:port/desc.xml"  engine-specific URL, engines opt in
//
// When only is non-empty, only that engine is tried (an engine prefix in the
// target overrides it).
func ResolveTarget(ctx context.Context, target, only string) (Device, error) {
	name, bare := SplitTarget(target)
	if name != "" {
		only = name
	}
	if only != "" {
		e, ok := Lookup(only)
		if !ok {
			return nil, fmt.Errorf("unknown engine %q (have: %s)", only, engineNames())
		}
		d, err := e.Resolve(ctx, bare)
		if err != nil {
			return nil, fmt.Errorf("%s: resolve %q: %w", e.Name(), bare, err)
		}
		return d, nil
	}

	var firstErr error
	for _, e := range registry {
		d, err := e.Resolve(ctx, bare)
		if err == nil {
			return d, nil
		}
		if err == ErrNotThisEngine || ctx.Err() != nil {
			continue
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	if firstErr != nil {
		return nil, fmt.Errorf("resolve %q: %w", target, firstErr)
	}
	return nil, fmt.Errorf("no engine can resolve target %q (tried: %s)", target, engineNames())
}

// SplitTarget splits an "engine:host" target into its engine name and the bare
// target. A prefix only counts when it names a registered engine, so URL schemes
// ("http://...") and bare IPv6-ish strings pass through untouched.
func SplitTarget(target string) (engineName, bare string) {
	i := strings.Index(target, ":")
	if i <= 0 {
		return "", target
	}
	if _, ok := Lookup(target[:i]); ok {
		return target[:i], target[i+1:]
	}
	return "", target
}

func engineNames() string {
	names := make([]string, len(registry))
	for i, e := range registry {
		names[i] = e.Name()
	}
	return strings.Join(names, ", ")
}

// Rank is an engine's registration index: lower is preferred when one TV
// answers on several engines. Unknown engines rank last.
func Rank(name string) int {
	for i, e := range registry {
		if e.Name() == name {
			return i
		}
	}
	return len(registry)
}

// PreferredPerHost collapses devices that share a host (one TV speaking several
// protocols, e.g. a Google TV that also runs a DLNA renderer) to the one whose
// engine ranks first, so each TV shows up once. also maps each kept device's
// Host to the other engines that answered for it. Order follows each host's
// first appearance.
func PreferredPerHost(devices []Device) (kept []Device, also map[string][]string) {
	idx := map[string]int{}
	also = map[string][]string{}
	for _, d := range devices {
		i, seen := idx[d.Host()]
		if !seen {
			idx[d.Host()] = len(kept)
			kept = append(kept, d)
			continue
		}
		if Rank(d.Engine()) < Rank(kept[i].Engine()) {
			also[d.Host()] = append(also[d.Host()], kept[i].Engine())
			kept[i] = d
		} else {
			also[d.Host()] = append(also[d.Host()], d.Engine())
		}
	}
	return kept, also
}
