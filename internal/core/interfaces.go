// Package core is movcaster's UI-agnostic orchestration layer. It owns everything
// a front-end needs to cast a file: list devices, plan a cast (probe + subtitle/
// codec strategy), start it, control it, and tear it down. It never imports a UI
// toolkit and never prints; progress is reported through Options.OnEvent and
// status is read back through the live Cast handle.
//
// Core talks to TVs only through the engine contract (internal/engine): it never
// imports a concrete engine. The CLI/TUI in package main is a thin client over
// this package; a future HTTP daemon or GUI would consume the same surface.
package core

import (
	"context"
	"time"

	"github.com/juliocesar/movcaster/internal/config"
	"github.com/juliocesar/movcaster/internal/engine"
	"github.com/juliocesar/movcaster/internal/mediaserver"
	"github.com/juliocesar/movcaster/internal/resume"
)

// DeviceFinder discovers renderers across all registered engines. The
// production impl wraps the engine registry; tests inject a fake to resolve
// devices without a real network.
type DeviceFinder interface {
	// Discover finds devices on the LAN. only restricts to one engine ("" = all).
	Discover(ctx context.Context, only string) ([]engine.Device, error)
	// Resolve builds a device from an explicit target (bare IP, host:port,
	// engine:host, or an engine-specific URL) without a broadcast search.
	Resolve(ctx context.Context, target, onlyEngine string) (engine.Device, error)
}

// MediaServer is the local HTTP server that the TV pulls media/subs from.
// *mediaserver.Server satisfies it; tests inject a fake.
type MediaServer interface {
	Start()
	Shutdown(ctx context.Context) error
	SetDirectPlay(filePath, mime string)
	SetTranscode(args []string)
	SetSubtitle(path, mime string)
	MediaURL() string
	SubURL() string
}

// Store persists cross-run state (the last device). Defaults to the config
// package; tests inject a fake to avoid touching the user's real config.
type Store interface {
	Load() config.Config
	Save(config.Config) error
}

// resumeStore persists per-file playback positions for the resume feature.
// *resume.Store satisfies it. It is supplied by the CLI (via Options.Resume) so
// that core unit tests stay hermetic; when nil, resume is simply disabled.
type resumeStore interface {
	Get(absPath string) time.Duration
	Set(absPath string, pos time.Duration) error
	Clear(absPath string) error
}

var _ resumeStore = (*resume.Store)(nil)

// Concrete impls satisfy the interfaces (compile-time checks).
var (
	_ DeviceFinder = realFinder{}
	_ MediaServer  = (*mediaserver.Server)(nil)
	_ Store        = configStore{}
)

// realFinder wraps the engine registry.
type realFinder struct{}

func (realFinder) Discover(ctx context.Context, only string) ([]engine.Device, error) {
	devices, errs := engine.DiscoverAll(ctx, only)
	// A single engine's failure is not fatal (a broken mDNS stack must not stop
	// DLNA discovery); only surface an error when nothing was found at all.
	if len(devices) == 0 && len(errs) > 0 {
		return nil, errs[0]
	}
	return devices, nil
}

func (realFinder) Resolve(ctx context.Context, target, onlyEngine string) (engine.Device, error) {
	return engine.ResolveTarget(ctx, target, onlyEngine)
}

// configStore wraps the config package.
type configStore struct{}

func (configStore) Load() config.Config        { return config.Load() }
func (configStore) Save(c config.Config) error { return config.Save(c) }

// realServer binds a production media server (typed as the interface).
func realServer(deviceHost string) (MediaServer, error) {
	return mediaserver.New(deviceHost)
}
