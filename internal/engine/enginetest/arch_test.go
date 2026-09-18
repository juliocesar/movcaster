package enginetest

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestCoreDoesNotImportEngines asserts that no package outside
// internal/engine/... and main imports a concrete engine. This is the actual
// architectural promise: adding a TV family is a new package plus one line in
// engine/all, touching nothing else. If this fails, fix the abstraction gap
// rather than special-casing an engine.
func TestCoreDoesNotImportEngines(t *testing.T) {
	concrete := regexp.MustCompile(`internal/engine/(dlna|googlecast|roku)($|/)`)
	for _, pkg := range []string{
		"github.com/juliocesar/movcaster/internal/core",
		"github.com/juliocesar/movcaster/internal/tui",
		"github.com/juliocesar/movcaster/internal/subs",
		"github.com/juliocesar/movcaster/internal/transcode",
		"github.com/juliocesar/movcaster/internal/mediaserver",
		"github.com/juliocesar/movcaster/internal/probe",
	} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if concrete.MatchString(dep) {
				t.Errorf("%s depends on the concrete engine %s", pkg, dep)
			}
		}
	}
}
