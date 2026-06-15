// Package renderer drives a DLNA MediaRenderer via AVTransport (transport control)
// and RenderingControl (volume). It builds the DIDL-Lite metadata, including the
// Samsung/LG sec:CaptionInfoEx subtitle reference webOS honors for text subs.
package renderer

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/huin/goupnp/dcps/av1"
	"github.com/juliocesar/movcaster/internal/discovery"
)

// Renderer is a control handle over a single discovered device.
type Renderer struct {
	Name string
	av   *av1.AVTransport1
	rc   *av1.RenderingControl1
}

// New wraps a discovered device.
func New(d discovery.Device) *Renderer {
	return &Renderer{Name: d.FriendlyName, av: d.AVTransport, rc: d.Rendering}
}

// Media describes what to cast.
type Media struct {
	URL      string        // http URL the TV will GET
	Title    string        // shown in DIDL dc:title
	MIME     string        // e.g. "video/mp4"
	Duration time.Duration // 0 = unknown
	Seekable bool          // true for direct-play (range), false for transcode pipe

	// Optional soft subtitle (text only). Empty URL = no soft sub.
	SubURL  string
	SubMIME string // "text/vtt" or "application/x-subrip"
	SubType string // "vtt" or "srt" (the sec:type attribute)
}

// SetMedia issues SetAVTransportURI with DIDL-Lite metadata for the media.
func (r *Renderer) SetMedia(ctx context.Context, m Media) error {
	meta := buildDIDL(m)
	return r.av.SetAVTransportURICtx(ctx, 0, m.URL, meta)
}

// buildDIDL constructs the CurrentURIMetaData string. goupnp XML-escapes it when
// embedding into the SOAP body, so this returns raw (unescaped) DIDL-Lite.
func buildDIDL(m Media) string {
	op := "00"
	flags := "01700000000000000000000000000000" // streaming, no seek
	if m.Seekable {
		op = "01"
		flags = "01500000000000000000000000000000" // byte-seekable
	}
	protocolInfo := fmt.Sprintf("http-get:*:%s:DLNA.ORG_OP=%s;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=%s", m.MIME, op, flags)

	dur := ""
	if m.Duration > 0 {
		dur = fmt.Sprintf(` duration="%s"`, formatDuration(m.Duration))
	}

	var sub strings.Builder
	if m.SubURL != "" {
		subURL := html.EscapeString(m.SubURL)
		sub.WriteString(fmt.Sprintf(`<res protocolInfo="http-get:*:%s:*">%s</res>`, m.SubMIME, subURL))
		sub.WriteString(fmt.Sprintf(`<sec:CaptionInfo sec:type="%s">%s</sec:CaptionInfo>`, m.SubType, subURL))
		sub.WriteString(fmt.Sprintf(`<sec:CaptionInfoEx sec:type="%s">%s</sec:CaptionInfoEx>`, m.SubType, subURL))
	}

	return `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
		`xmlns:sec="http://www.sec.co.kr/" ` +
		`xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">` +
		`<item id="0" parentID="-1" restricted="1">` +
		`<dc:title>` + html.EscapeString(m.Title) + `</dc:title>` +
		`<upnp:class>object.item.videoItem</upnp:class>` +
		`<res protocolInfo="` + protocolInfo + `"` + dur + `>` + html.EscapeString(m.URL) + `</res>` +
		sub.String() +
		`</item></DIDL-Lite>`
}

func (r *Renderer) Play(ctx context.Context) error  { return r.av.PlayCtx(ctx, 0, "1") }
func (r *Renderer) Pause(ctx context.Context) error { return r.av.PauseCtx(ctx, 0) }
func (r *Renderer) Stop(ctx context.Context) error  { return r.av.StopCtx(ctx, 0) }

// Seek jumps to an absolute position using REL_TIME.
func (r *Renderer) Seek(ctx context.Context, pos time.Duration) error {
	return r.av.SeekCtx(ctx, 0, "REL_TIME", formatDuration(pos))
}

// Position returns the current playback position and track duration.
func (r *Renderer) Position(ctx context.Context) (pos, dur time.Duration, err error) {
	_, durStr, _, _, relStr, _, _, _, err := r.av.GetPositionInfoCtx(ctx, 0)
	if err != nil {
		return 0, 0, err
	}
	return parseDuration(relStr), parseDuration(durStr), nil
}

// TransportState returns e.g. PLAYING, PAUSED_PLAYBACK, STOPPED, TRANSITIONING.
func (r *Renderer) TransportState(ctx context.Context) (string, error) {
	state, _, _, err := r.av.GetTransportInfoCtx(ctx, 0)
	return state, err
}

// HasVolume reports whether the device exposes RenderingControl.
func (r *Renderer) HasVolume() bool { return r.rc != nil }

// Volume returns the master volume (0-100).
func (r *Renderer) Volume(ctx context.Context) (int, error) {
	if r.rc == nil {
		return 0, fmt.Errorf("device has no volume control")
	}
	v, err := r.rc.GetVolumeCtx(ctx, 0, "Master")
	return int(v), err
}

// SetVolume sets the master volume (0-100, clamped).
func (r *Renderer) SetVolume(ctx context.Context, v int) error {
	if r.rc == nil {
		return fmt.Errorf("device has no volume control")
	}
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return r.rc.SetVolumeCtx(ctx, 0, "Master", uint16(v))
}

// Mute sets the master mute state.
func (r *Renderer) Mute(ctx context.Context, on bool) error {
	if r.rc == nil {
		return fmt.Errorf("device has no volume control")
	}
	return r.rc.SetMuteCtx(ctx, 0, "Master", on)
}

// formatDuration renders a duration as H:MM:SS (UPnP REL_TIME / duration format).
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d / time.Second)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}

// parseDuration parses "H:MM:SS" / "HH:MM:SS(.fff)" into a Duration. Returns 0 on failure.
func parseDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" || s == "NOT_IMPLEMENTED" {
		return 0
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	var h, m, sec int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return 0
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return 0
	}
	if _, err := fmt.Sscanf(parts[2], "%d", &sec); err != nil {
		return 0
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second
}
