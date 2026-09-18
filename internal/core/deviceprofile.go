package core

import (
	"github.com/juliocesar/movcaster/internal/config"
	"github.com/juliocesar/movcaster/internal/engine"
)

// DeviceProfile returns the device's effective profile: the engine default
// merged with any config override for its "engine:host" key. This is what makes
// a new TV genuinely cheap — ship a conservative engine default, then let
// --probe-device widen it per device.
func (a *App) DeviceProfile(dev engine.Device) engine.Profile {
	prof := dev.Profile()
	overrides := a.store.Load().DeviceProfiles
	if len(overrides) == 0 {
		return prof
	}
	o, ok := overrides[profileKey(dev)]
	if !ok {
		return prof
	}
	return applyOverride(prof, o)
}

func profileKey(dev engine.Device) string { return dev.Engine() + ":" + dev.Host() }

// applyOverride replaces each axis the override sets; empty lists keep the
// engine default for that axis.
func applyOverride(p engine.Profile, o config.ProfileOverride) engine.Profile {
	if len(o.Containers) > 0 {
		p.Containers = listToSet(o.Containers)
	}
	if len(o.VideoCodecs) > 0 {
		p.VideoCodecs = listToSet(o.VideoCodecs)
	}
	if len(o.AudioCodecs) > 0 {
		p.AudioCodecs = listToSet(o.AudioCodecs)
	}
	if o.MaxVideoHeight > 0 {
		p.MaxVideoHeight = o.MaxVideoHeight
	}
	if o.MaxAudioChannels > 0 {
		p.MaxAudioChannels = o.MaxAudioChannels
	}
	if p.Name != "" {
		p.Name += ", device override"
	}
	return p
}

func listToSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
