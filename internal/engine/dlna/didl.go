package dlna

import (
	"fmt"
	"html"
	"strings"

	"github.com/juliocesar/movcaster/internal/engine"
)

// buildDIDL constructs the CurrentURIMetaData string for SetAVTransportURI.
// goupnp XML-escapes it when embedding into the SOAP body, so this returns raw
// (unescaped) DIDL-Lite. The DLNA.ORG_FLAGS values and the sec:CaptionInfo /
// sec:CaptionInfoEx pair (the Samsung/LG extension webOS honors for TEXT subs)
// were arrived at empirically — do not simplify. res@duration matters for a
// transcode: the stream is an empty-moov fragmented MP4 with no in-stream
// duration, and without the hint the TV's seek bar races.
func buildDIDL(m engine.Media) string {
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
	if m.Subtitle != nil && m.Subtitle.URL != "" {
		subURL := html.EscapeString(m.Subtitle.URL)
		secType := m.Subtitle.Format.String() // "srt" / "vtt"
		sub.WriteString(fmt.Sprintf(`<res protocolInfo="http-get:*:%s:*">%s</res>`, m.Subtitle.MIME, subURL))
		sub.WriteString(fmt.Sprintf(`<sec:CaptionInfo sec:type="%s">%s</sec:CaptionInfo>`, secType, subURL))
		sub.WriteString(fmt.Sprintf(`<sec:CaptionInfoEx sec:type="%s">%s</sec:CaptionInfoEx>`, secType, subURL))
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
