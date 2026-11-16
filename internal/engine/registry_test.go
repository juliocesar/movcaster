package engine

import (
	"context"
	"testing"
)

// fakeEngine resolves any bare-IP target that matches its host.
type fakeEngine struct {
	name string
	host string
	dev  Device
}

func (f fakeEngine) Name() string                               { return f.name }
func (f fakeEngine) Discover(context.Context) ([]Device, error) { return nil, nil }
func (f fakeEngine) DefaultProfile() Profile                    { return Profile{} }
func (f fakeEngine) Resolve(_ context.Context, target string) (Device, error) {
	if target == f.host {
		return f.dev, nil
	}
	return nil, ErrNotThisEngine
}

type fakeDevice struct{ name, engine string }

func (d fakeDevice) Name() string                          { return d.name }
func (d fakeDevice) Host() string                          { return "10.0.0.1" }
func (d fakeDevice) Addr() string                          { return "10.0.0.1:1" }
func (d fakeDevice) Engine() string                        { return d.engine }
func (d fakeDevice) Profile() Profile                      { return Profile{} }
func (d fakeDevice) Open(context.Context) (Session, error) { return nil, ErrNotThisEngine }

// withRegistry swaps the global registry for the test's engines.
func withRegistry(t *testing.T, engines ...Engine) {
	t.Helper()
	old := registry
	registry = engines
	t.Cleanup(func() { registry = old })
}

func TestResolveTargetTriesEnginesInOrder(t *testing.T) {
	a := fakeEngine{name: "aaa", host: "10.0.0.9"}
	b := fakeEngine{name: "bbb", host: "10.0.0.5", dev: fakeDevice{name: "TV", engine: "bbb"}}
	withRegistry(t, a, b)

	d, err := ResolveTarget(context.Background(), "10.0.0.5", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "TV" || d.Engine() != "bbb" {
		t.Fatalf("got %s/%s", d.Name(), d.Engine())
	}
}

func TestResolveTargetEnginePrefix(t *testing.T) {
	a := fakeEngine{name: "aaa", host: "10.0.0.5", dev: fakeDevice{name: "A", engine: "aaa"}}
	b := fakeEngine{name: "bbb", host: "10.0.0.5", dev: fakeDevice{name: "B", engine: "bbb"}}
	withRegistry(t, a, b)

	d, err := ResolveTarget(context.Background(), "bbb:10.0.0.5", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "B" {
		t.Fatalf("prefix must restrict to bbb, got %s", d.Name())
	}
	// An unknown prefix is not an engine name: it passes through as the target.
	if name, bare := SplitTarget("http://10.0.0.5:1122/desc.xml"); name != "" || bare != "http://10.0.0.5:1122/desc.xml" {
		t.Fatalf("URL target mangled: %q %q", name, bare)
	}
}

func TestResolveTargetOnlyRestricts(t *testing.T) {
	a := fakeEngine{name: "aaa", host: "10.0.0.5", dev: fakeDevice{name: "A", engine: "aaa"}}
	b := fakeEngine{name: "bbb", host: "10.0.0.5", dev: fakeDevice{name: "B", engine: "bbb"}}
	withRegistry(t, a, b)

	d, err := ResolveTarget(context.Background(), "10.0.0.5", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "B" {
		t.Fatalf("only=bbb must pick B, got %s", d.Name())
	}
	if _, err := ResolveTarget(context.Background(), "10.0.0.5", "nope"); err == nil {
		t.Fatal("unknown engine name must error")
	}
}

func TestDiscoverAllMergesAndCollectsErrors(t *testing.T) {
	ok := fakeEngine{name: "ok", dev: fakeDevice{}}
	withRegistry(t, ok, failingEngine{})
	devs, errs := DiscoverAll(context.Background(), "")
	if len(devs) != 0 { // fakeEngine.Discover returns nothing
		t.Fatalf("devices = %v", devs)
	}
	if len(errs) != 1 {
		t.Fatalf("one engine failed, errs = %v", errs)
	}
}

type failingEngine struct{}

func (failingEngine) Name() string { return "bad" }
func (failingEngine) Discover(context.Context) ([]Device, error) {
	return nil, context.DeadlineExceeded
}
func (failingEngine) Resolve(context.Context, string) (Device, error) { return nil, ErrNotThisEngine }
func (failingEngine) DefaultProfile() Profile                         { return Profile{} }

func TestSubFormat(t *testing.T) {
	if SubFormatSRT.Ext() != ".srt" || SubFormatVTT.Ext() != ".vtt" {
		t.Fatal("ext")
	}
	if SubFormatSRT.MIME() != "text/srt" || SubFormatVTT.MIME() != "text/vtt" {
		t.Fatal("mime")
	}
	if (Profile{}).SoftSubFormat() != SubFormatNone {
		t.Fatal("empty profile must have no soft format")
	}
	p := Profile{SubFormats: []SubFormat{SubFormatVTT, SubFormatSRT}}
	if p.SoftSubFormat() != SubFormatVTT {
		t.Fatal("preference order")
	}
}

type hostDevice struct{ name, host, engine string }

func (d hostDevice) Name() string                          { return d.name }
func (d hostDevice) Host() string                          { return d.host }
func (d hostDevice) Addr() string                          { return d.host + ":1" }
func (d hostDevice) Engine() string                        { return d.engine }
func (d hostDevice) Profile() Profile                      { return Profile{} }
func (d hostDevice) Open(context.Context) (Session, error) { return nil, ErrNotThisEngine }

// A TV answering on two engines shows up once, under the engine registered
// first, whichever order discovery returned them in; other TVs are untouched.
func TestPreferredPerHost(t *testing.T) {
	withRegistry(t, fakeEngine{name: "native"}, fakeEngine{name: "generic"})
	devs := []Device{
		hostDevice{"TCL generic", "10.0.0.2", "generic"},
		hostDevice{"LG", "10.0.0.3", "generic"},
		hostDevice{"TCL native", "10.0.0.2", "native"},
	}
	kept, also := PreferredPerHost(devs)
	if len(kept) != 2 || kept[0].Name() != "TCL native" || kept[1].Name() != "LG" {
		t.Fatalf("kept = %v", kept)
	}
	if got := also["10.0.0.2"]; len(got) != 1 || got[0] != "generic" {
		t.Fatalf("also[TCL] = %v", got)
	}
	if len(also["10.0.0.3"]) != 0 {
		t.Fatalf("also[LG] = %v", also["10.0.0.3"])
	}
}
