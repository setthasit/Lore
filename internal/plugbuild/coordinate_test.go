package plugbuild

import (
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

func TestParseCoordinateAcceptsModuleForms(t *testing.T) {
	cases := []struct {
		raw  string
		want Coordinate
	}{
		{
			raw:  "github.com/jdoe/lore-linear@v0.3.1",
			want: Coordinate{Module: "github.com/jdoe/lore-linear", Version: "v0.3.1", Package: "linear"},
		},
		{
			raw:  "github.com/acme/lore-crm/v2@v2.0.1",
			want: Coordinate{Module: "github.com/acme/lore-crm/v2", Version: "v2.0.1", Package: "crm"},
		},
		{
			raw:  "github.com/acme/notion@v1.0.0",
			want: Coordinate{Module: "github.com/acme/notion", Version: "v1.0.0", Package: "notion"},
		},
		{
			raw:  "github.com/acme/lore-acme.crm@v1.0.0=acmecrm",
			want: Coordinate{Module: "github.com/acme/lore-acme.crm", Version: "v1.0.0", Package: "acmecrm"},
		},
	}

	for _, c := range cases {
		got, err := ParseCoordinate(c.raw)
		if err != nil {
			t.Errorf("ParseCoordinate(%q) = %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseCoordinate(%q) = %+v, want %+v", c.raw, got, c.want)
		}
	}
}

// The failure a user is most likely to hit is a repository whose name is not an
// identifier, and the only recovery is the suffix, so every one of these must
// name it rather than describing the problem abstractly.
func TestParseCoordinateAsksForAnExplicitPackage(t *testing.T) {
	for _, raw := range []string{
		"github.com/acme/lore-acme.crm@v1.0.0",
		"github.com/acme/lore-acme-crm@v1.0.0",
		"github.com/acme/lore-2fa@v1.0.0",
		"github.com/acme/lore-range@v1.0.0", // a Go keyword
		"github.com/acme/lore-os@v1.0.0",    // already imported by the generated root
		"github.com/acme/lore-plugins@v1.0.0",
	} {
		_, err := ParseCoordinate(raw)
		if err == nil {
			t.Errorf("ParseCoordinate(%q) guessed a package name instead of asking for one", raw)
			continue
		}
		if internalerror.KindOf(err) != internalerror.KindBadRequest {
			t.Errorf("ParseCoordinate(%q) kind = %v, want bad request", raw, internalerror.KindOf(err))
		}
		if !strings.Contains(err.Error(), "=acmecrm") {
			t.Errorf("ParseCoordinate(%q) = %q, want the =<package> suffix spelled out", raw, err)
		}
	}
}

func TestParseCoordinateRejectsUnbuildableSpecs(t *testing.T) {
	cases := map[string]string{
		"":                                   "empty",
		"github.com/jdoe/lore-linear":        "no version",
		"github.com/jdoe/lore-linear@":       "no version",
		"github.com/jdoe/lore-linear@latest": "exact module version tag",
		"lore-linear@v0.1.0":                 "needs a host",
		"./bin/lore-scratch":                 "not paths",
		"github.com/jdoe/lore-linear@v0.3.1=not an identifier": "not a Go package name",
	}

	for raw, want := range cases {
		_, err := ParseCoordinate(raw)
		if err == nil {
			t.Errorf("ParseCoordinate(%q) accepted an unbuildable coordinate", raw)
			continue
		}
		if internalerror.KindOf(err) != internalerror.KindBadRequest {
			t.Errorf("ParseCoordinate(%q) kind = %v, want bad request", raw, internalerror.KindOf(err))
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ParseCoordinate(%q) = %q, want it to mention %q", raw, err, want)
		}
	}
}

// A local coordinate is not a mistake, it is the other mode: the message has to
// point at it instead of only refusing.
func TestParseCoordinatePointsALocalPathAtTheExternalMode(t *testing.T) {
	_, err := ParseCoordinate("./bin/lore-scratch")
	if err == nil {
		t.Fatal("ParseCoordinate accepted a path as a module coordinate")
	}
	for _, want := range []string{"out of process", "plugins:", "lore.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}
