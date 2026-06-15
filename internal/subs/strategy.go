// Package subs decides how to deliver subtitles for a cast, implementing the
// decision tree from the plan: sidecar/text -> soft; bitmap -> mux-as-soft or burn.
package subs

import (
	"fmt"

	"github.com/juliocesar/movcaster/internal/probe"
)

// Kind is the chosen subtitle delivery strategy.
type Kind int

const (
	None        Kind = iota // no subtitles
	SoftSidecar             // serve an external text file as soft subs
	SoftExtract             // extract an embedded text track to vtt, serve as soft
	MuxSoft                 // remux a bitmap track into a fresh container (M6a, experimental)
	Burn                    // burn a bitmap (or text) track into the video (M6b)
)

func (k Kind) String() string {
	switch k {
	case SoftSidecar:
		return "soft (sidecar)"
	case SoftExtract:
		return "soft (extracted text)"
	case MuxSoft:
		return "soft (muxed bitmap)"
	case Burn:
		return "burn-in"
	default:
		return "none"
	}
}

// Request holds the inputs to the decision.
type Request struct {
	Info        *probe.MediaInfo
	SidecarPath string // resolved sidecar/--sub path, or ""
	NoSubs      bool
	ForceBurn   bool // --burn
	ForceSoft   bool // --soft (error if chosen track is bitmap)
	MuxSoftTry  bool // --mux-soft: remux a bitmap track as soft instead of burning (experimental)
	TrackIndex  int  // explicit subtitle stream index (s:N); -1 = auto
}

// Decision is the outcome of Decide.
type Decision struct {
	Kind        Kind
	SidecarPath string          // for SoftSidecar
	Track       *probe.SubTrack // for SoftExtract/MuxSoft/Burn
	Reason      string
}

// Decide resolves the subtitle strategy. It never executes anything.
func Decide(req Request) (Decision, error) {
	if req.NoSubs {
		return Decision{Kind: None, Reason: "--no-subs"}, nil
	}

	// An explicit sidecar/--sub always wins (text, served directly).
	if req.SidecarPath != "" {
		return Decision{Kind: SoftSidecar, SidecarPath: req.SidecarPath, Reason: "sidecar/--sub provided"}, nil
	}

	var subsList []probe.SubTrack
	if req.Info != nil {
		subsList = req.Info.Subtitles
	}
	if len(subsList) == 0 {
		return Decision{Kind: None, Reason: "no subtitle tracks found"}, nil
	}

	// Resolve which track we're acting on.
	track, err := selectTrack(subsList, req)
	if err != nil {
		return Decision{}, err
	}

	// Honor forcing flags first.
	if req.ForceSoft {
		if track.Kind != probe.SubText {
			return Decision{}, fmt.Errorf("--soft requires a text subtitle track, but track %d is %s (%s)", track.SubIndex, track.Kind, track.Codec)
		}
		return Decision{Kind: SoftExtract, Track: track, Reason: "--soft on text track"}, nil
	}
	if req.ForceBurn {
		return Decision{Kind: Burn, Track: track, Reason: "--burn"}, nil
	}

	// Auto: text -> soft. Bitmap -> burn by default (webOS does not demux embedded
	// subs over DLNA, see plan 0.4); --mux-soft opts into the experimental soft path.
	switch track.Kind {
	case probe.SubText:
		return Decision{Kind: SoftExtract, Track: track, Reason: "embedded text track"}, nil
	case probe.SubBitmap:
		if req.MuxSoftTry {
			return Decision{Kind: MuxSoft, Track: track, Reason: "--mux-soft on bitmap track (experimental)"}, nil
		}
		return Decision{Kind: Burn, Track: track, Reason: "bitmap track (burn-in)"}, nil
	default:
		return Decision{Kind: None, Reason: fmt.Sprintf("track %d codec %q not handled", track.SubIndex, track.Codec)}, nil
	}
}

// selectTrack picks the subtitle track to act on. With an explicit index, it must
// exist. Otherwise it prefers a text track (best UX: soft), else the default
// track, else the first.
func selectTrack(list []probe.SubTrack, req Request) (*probe.SubTrack, error) {
	if req.TrackIndex >= 0 {
		for i := range list {
			if list[i].SubIndex == req.TrackIndex {
				return &list[i], nil
			}
		}
		return nil, fmt.Errorf("subtitle track %d not found (%d tracks available)", req.TrackIndex, len(list))
	}

	// Auto-selection. When burning is forced we don't bias toward text.
	if !req.ForceBurn {
		for i := range list {
			if list[i].Kind == probe.SubText {
				return &list[i], nil
			}
		}
	}
	for i := range list {
		if list[i].Default {
			return &list[i], nil
		}
	}
	return &list[0], nil
}
