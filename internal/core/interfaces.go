// Package core is movcaster's UI-agnostic orchestration layer. It owns everything
// a front-end needs to cast a file: list devices, plan a cast (probe + subtitle/
// codec strategy), start it, control it, and tear it down. It never imports a UI
// toolkit and never prints; progress is reported through Options.OnEvent and
// status is read back through the live Cast handle.
//
// The CLI/TUI in package main is a thin client over this package. A future HTTP
// daemon or GUI would consume the same surface.
package core

import (
	"context"
	"net/url"
	"time"

	"github.com/juliocesar/movcaster/internal/config"
	"github.com/juliocesar/movcaster/internal/discovery"
	"github.com/juliocesar/movcaster/internal/mediaserver"
	"github.com/juliocesar/movcaster/internal/renderer"
)

// DeviceFinder discovers DLNA renderers. The production impl wraps the discovery
// package; tests inject a fake to resolve devices without a real network.
type DeviceFinder interface {
	Discover(ctx context.Context) ([]discovery.Device, error)
	FindByURL(ctx context.Context, loc *url.URL) (*discovery.Device, error)
}

// RendererControl is the transport/volume control surface of a renderer.
// *renderer.Renderer satisfies it; tests inject a fake.
type RendererControl interface {
	SetMedia(ctx context.Context, m renderer.Media) error
	Play(ctx context.Context) error
	Pause(ctx context.Context) error
	Stop(ctx context.Context) error
	Seek(ctx context.Context, pos time.Duration) error
	Position(ctx context.Context) (pos, dur time.Duration, err error)
	TransportState(ctx context.Context) (string, error)
	HasVolume() bool
	Volume(ctx context.Context) (int, error)
	SetVolume(ctx context.Context, v int) error
	Mute(ctx context.Context, on bool) error
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

// Concrete impls satisfy the interfaces (compile-time checks).
var (
	_ DeviceFinder    = realFinder{}
	_ RendererControl = (*renderer.Renderer)(nil)
	_ MediaServer     = (*mediaserver.Server)(nil)
	_ Store           = configStore{}
)

// realFinder wraps the discovery package's SSDP functions.
type realFinder struct{}

func (realFinder) Discover(ctx context.Context) ([]discovery.Device, error) {
	return discovery.Discover(ctx)
}
func (realFinder) FindByURL(ctx context.Context, loc *url.URL) (*discovery.Device, error) {
	return discovery.FindByURL(ctx, loc)
}

// configStore wraps the config package.
type configStore struct{}

func (configStore) Load() config.Config        { return config.Load() }
func (configStore) Save(c config.Config) error { return config.Save(c) }

// realServer binds a production media server (typed as the interface).
func realServer(deviceHost string) (MediaServer, error) {
	return mediaserver.New(deviceHost)
}
