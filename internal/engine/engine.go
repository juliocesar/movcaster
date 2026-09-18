// Package engine defines the contract between movcaster's core and a TV
// protocol family (DLNA, Google Cast, ...). Core depends only on this package;
// concrete engines live under internal/engine/<name> and register themselves via
// internal/engine/all. Adding a TV family is a new package plus one registration
// line — nothing in core, subs, transcode, mediaserver, tui or main changes.
package engine

import (
	"context"
	"errors"
	"time"
)

// ErrNotThisEngine is returned by Engine.Resolve when the target is not
// addressable by that protocol family, letting the registry try the next one.
var ErrNotThisEngine = errors.New("target is not addressable by this engine")

// ErrNotSeekable is returned by Session.Seek when the loaded media's transport
// does not support in-stream seeking (callers fall back to seek-restart).
var ErrNotSeekable = errors.New("loaded media is not seekable")

// ErrNoVolume is returned by the volume methods of a device without volume control.
var ErrNoVolume = errors.New("device has no volume control")

// Engine is one protocol family. Implementations are stateless and cheap.
type Engine interface {
	// Name is the stable identifier used in config, -t prefixes and --engine.
	Name() string // "dlna", "googlecast", "roku"

	// Discover finds every device of this family on the LAN. It must respect
	// ctx and must not return an error merely because nothing was found.
	Discover(ctx context.Context) ([]Device, error)

	// Resolve builds a Device from a user-supplied target (bare IP, host:port,
	// or a protocol-specific URL) without a broadcast search. It returns
	// ErrNotThisEngine when the target is not addressable by this family, which
	// lets the registry try the next engine.
	Resolve(ctx context.Context, target string) (Device, error)

	// DefaultProfile is the engine's conservative capability default, used when
	// planning without a resolved device (--info with no -t) and as the base a
	// per-device override merges onto.
	DefaultProfile() Profile
}

// Device is a discovered renderer. Cheap to hold, no live connection.
type Device interface {
	Name() string   // friendly name for the UI
	Host() string   // bare IP: media-server binding and config key
	Addr() string   // host:port, for display and logs
	Engine() string // owning engine's Name()
	Profile() Profile

	// Open establishes a control session. The caller always Closes it.
	Open(ctx context.Context) (Session, error)
}

// Session is a live control channel to one device. Implementations must be
// safe for concurrent use.
type Session interface {
	// Load points the device at the media and returns as soon as the device has
	// ACCEPTED it. It must NOT block waiting for buffering to finish. Most
	// devices begin fetching (and often auto-playing) at this point.
	Load(ctx context.Context, m Media) error

	// Play ensures playback is running. It is allowed to block while the device
	// finishes buffering the media Load handed it, so callers run it off the
	// critical path. Calling Play on an already-playing device is a no-op.
	Play(ctx context.Context) error

	Pause(ctx context.Context) error
	Stop(ctx context.Context) error

	// Seek jumps to an absolute position. Only meaningful when the currently
	// loaded Media had Seekable set. Engines return ErrNotSeekable otherwise.
	Seek(ctx context.Context, pos time.Duration) error

	// Status is one snapshot: state, position, duration. Engines that push
	// status (Cast) serve it from cache; engines that poll (DLNA) do the I/O.
	Status(ctx context.Context) (State, error)

	HasVolume() bool
	Volume(ctx context.Context) (int, error)    // 0-100
	SetVolume(ctx context.Context, v int) error // 0-100, engine clamps
	Mute(ctx context.Context, on bool) error

	Close() error
}

// PlayerState is the normalized transport state. Engines map their native
// vocabulary onto these, so core and the TUI never string-match a protocol.
type PlayerState int

const (
	StateUnknown   PlayerState = iota
	StateIdle                  // nothing loaded
	StateBuffering             // loaded, not yet playing (DLNA TRANSITIONING, Cast BUFFERING)
	StatePlaying
	StatePaused
	StateStopped
	StateFinished // played to the end: drives auto-advance
)

func (s PlayerState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateBuffering:
		return "buffering"
	case StatePlaying:
		return "playing"
	case StatePaused:
		return "paused"
	case StateStopped:
		return "stopped"
	case StateFinished:
		return "finished"
	default:
		return "unknown"
	}
}

// State is one transport snapshot.
type State struct {
	Player   PlayerState
	Pos, Dur time.Duration
	// Native is the raw protocol state, for MOVCASTER_VERBOSE logging only.
	// Nothing may branch on it.
	Native string
}

// Media describes what a Session should play. It is protocol-neutral: engines
// translate it to DIDL-Lite, a Cast LOAD, or whatever their device speaks.
type Media struct {
	URL      string
	Title    string
	MIME     string
	Duration time.Duration // 0 = unknown

	// Seekable means the transport supports in-stream seeking (a byte-range
	// direct-play file). A transcode pipe is not seekable and the caller does
	// seek-restart instead.
	Seekable bool

	// StartAt asks the device to begin at this offset. Engines that support it
	// (Cast's LOAD currentTime) honor it; engines that do not (DLNA) ignore it
	// and the caller seeks after Play. Callers must tolerate both: check
	// Profile.HonorsStartAt.
	StartAt time.Duration

	Subtitle *Subtitle // nil = none
}

// SubFormat is a soft-subtitle delivery format.
type SubFormat int

const (
	SubFormatNone SubFormat = iota
	SubFormatSRT
	SubFormatVTT
)

func (f SubFormat) String() string {
	switch f {
	case SubFormatSRT:
		return "srt"
	case SubFormatVTT:
		return "vtt"
	default:
		return "none"
	}
}

// Ext is the file extension for the format (empty for SubFormatNone).
func (f SubFormat) Ext() string {
	switch f {
	case SubFormatSRT:
		return ".srt"
	case SubFormatVTT:
		return ".vtt"
	default:
		return ""
	}
}

// MIME is the HTTP content type the format is served with. SRT keeps the
// "text/srt" the LG path was verified with (not the IANA application/x-subrip).
func (f SubFormat) MIME() string {
	switch f {
	case SubFormatSRT:
		return "text/srt"
	case SubFormatVTT:
		return "text/vtt"
	default:
		return ""
	}
}

// Subtitle is a soft subtitle riding along with the media.
type Subtitle struct {
	URL      string
	Format   SubFormat
	MIME     string // see SubFormat.MIME
	Language string // BCP-47 where known, else the raw ffprobe tag
	Label    string // display name, e.g. "English"
}
