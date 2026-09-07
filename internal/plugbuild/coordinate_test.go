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
		if !internalerror.IsBadRequest(err) {
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
		"github.com/jdoe/lore-linear@v1":     "exact module version tag",
		"github.com/jdoe/lore-linear@v1.2":   "exact module version tag",
		"github.com/jdoe/lore-linear@vfoo":   "exact module version tag",
		"github.com/jdoe/lore-linear@main":   "exact module version tag",
		"lore-linear@v0.1.0":                 "needs a host",
		"lore-linear":                        "needs a host",
		"=pkg":                               "it names no module",
		"a b":                                "whitespace or quotes",
		"github.com/jdoe/lore-linear@v0.3.1=not an identifier": "not a Go package name",

		"./bin/lore-scratch": "not paths",
		"../lore-scratch":    "not paths",
		"/opt/lore-scratch":  "not paths",
		"~/lore-scratch":     "not paths",
		`.\lore-scratch`:     "not paths",
		`..\lore-scratch`:    "not paths",
		".":                  "begins with a host",
		"~":                  "begins with a host",
		".hidden/x":          "begins with a host",
		"~user/x":            "begins with a host",
		".hidden/x@v1.0.0":   "begins with a host",
		"~user/x@v1.0.0":     "begins with a host",
		"-C/x@v1.0.0":        "begins with a host",
		"--flag/x@v1.0.0":    "begins with a host",
	}

	for raw, want := range cases {
		_, err := ParseCoordinate(raw)
		if err == nil {
			t.Errorf("ParseCoordinate(%q) accepted an unbuildable coordinate", raw)
			continue
		}
		if !internalerror.IsBadRequest(err) {
			t.Errorf("ParseCoordinate(%q) kind = %v, want bad request", raw, internalerror.KindOf(err))
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ParseCoordinate(%q) = %q, want it to mention %q", raw, err, want)
		}
	}
}
