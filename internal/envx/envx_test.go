package envx_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/envx"
	"github.com/setthasit/Lore/internal/errors/internalerror"
)

const (
	field      = "providers[gateway].base_url"
	fakeToken  = "fake-not-a-real-token"
	missingVar = "LORE_ENVX_MISSING"
)

func setFakeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LORE_ENVX_HOST", "gateway.acme.dev")
	t.Setenv("LORE_ENVX_TENANT", "acme-crm")
	t.Setenv("LORE_ENVX_TOKEN", fakeToken)
	t.Setenv(missingVar, "")
	if err := os.Unsetenv(missingVar); err != nil {
		t.Fatalf("unset %s: %v", missingVar, err)
	}
}

func TestExpand(t *testing.T) {
	setFakeEnv(t)

	cases := map[string]struct {
		raw  string
		want string
	}{
		"a value without an expansion is unchanged": {
			raw:  "https://api.acme.dev/v1",
			want: "https://api.acme.dev/v1",
		},
		"one expansion": {
			raw:  "${env:LORE_ENVX_HOST}",
			want: "gateway.acme.dev",
		},
		"several expansions join with the text between them": {
			raw:  "${env:LORE_ENVX_HOST}/v1/${env:LORE_ENVX_TENANT}",
			want: "gateway.acme.dev/v1/acme-crm",
		},
		"an expansion surrounded by text": {
			raw:  "https://${env:LORE_ENVX_HOST}:8443/v1",
			want: "https://gateway.acme.dev:8443/v1",
		},
		"an escaped expansion stays literal and reads no variable": {
			raw:  "$${env:" + missingVar + "}",
			want: "${env:" + missingVar + "}",
		},
		"an escape beside a real expansion": {
			raw:  "${env:LORE_ENVX_HOST}/$${env:" + missingVar + "}",
			want: "gateway.acme.dev/${env:" + missingVar + "}",
		},
		"only the $$ directly before { escapes": {
			raw:  "$$${env:" + missingVar + "}",
			want: "$${env:" + missingVar + "}",
		},
		"$$ not before { is left alone": {
			raw:  "pa$$word$$$$",
			want: "pa$$word$$$$",
		},
		"a literal ending in a bare $": {
			raw:  "price$",
			want: "price$",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := envx.Expand(field, c.raw)
			if err != nil {
				t.Fatalf("Expand(%q) error = %v", c.raw, err)
			}
			if got != c.want {
				t.Errorf("Expand(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

func TestExpandNamesListsTheVariablesReadInOrder(t *testing.T) {
	setFakeEnv(t)

	cases := map[string]struct {
		raw  string
		want []string
	}{
		"several expansions, in the order written": {
			raw:  "${env:LORE_ENVX_TENANT}/v1/${env:LORE_ENVX_HOST}",
			want: []string{"LORE_ENVX_TENANT", "LORE_ENVX_HOST"},
		},
		"an escaped expansion reads nothing": {
			raw:  "$${env:" + missingVar + "}",
			want: nil,
		},
		"an escape beside a real expansion names only the real one": {
			raw:  "${env:LORE_ENVX_HOST}/$${env:" + missingVar + "}",
			want: []string{"LORE_ENVX_HOST"},
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, got, err := envx.ExpandNames(field, c.raw)
			if err != nil {
				t.Fatalf("ExpandNames(%q) error = %v", c.raw, err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("ExpandNames(%q) names = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

func TestExpandRefuses(t *testing.T) {
	setFakeEnv(t)

	malformed := []string{envx.Form, "$${", envx.NameRule}
	unset := []string{missingVar}
	cases := map[string]struct {
		raw      string
		want     []string
		unquoted string
	}{
		"a bare ${VAR}": {
			raw:      "${LORE_GATEWAY_URL}",
			want:     malformed,
			unquoted: "LORE_GATEWAY_URL",
		},
		"a default after the name": {
			raw:      "${env:LORE_GATEWAY_URL:-" + fakeToken + "}",
			want:     malformed,
			unquoted: "LORE_GATEWAY_URL",
		},
		"an empty name": {
			raw:      "https://${env:}/v1",
			want:     malformed,
			unquoted: "/v1",
		},
		"a lower-case name": {
			raw:      "${env:lore_gateway_url}",
			want:     malformed,
			unquoted: "lore_gateway_url",
		},
		"a name starting with a digit": {
			raw:      "${env:1LORE_GATEWAY_URL}",
			want:     malformed,
			unquoted: "1LORE_GATEWAY_URL",
		},
		"an unterminated expansion": {
			raw:      "https://${env:LORE_ENVX_HOST",
			want:     malformed,
			unquoted: "LORE_ENVX_HOST",
		},
		"a malformed expansion after a set secret": {
			raw:      "Bearer ${env:LORE_ENVX_TOKEN} ${LORE_GATEWAY_URL}",
			want:     malformed,
			unquoted: "LORE_GATEWAY_URL",
		},
		"an unset variable": {
			raw:      "https://${env:" + missingVar + "}/v1",
			want:     unset,
			unquoted: "/v1",
		},
		"an unset variable after a set secret": {
			raw:      "Bearer ${env:LORE_ENVX_TOKEN} ${env:" + missingVar + "}",
			want:     unset,
			unquoted: "${env:" + missingVar + "}",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := envx.Expand(field, c.raw)
			if err == nil {
				t.Fatalf("Expand(%q) = %q, want a refusal", c.raw, got)
			}
			if !internalerror.IsBadRequest(err) {
				t.Errorf("kind = %v, want bad request", internalerror.KindOf(err))
			}
			for _, want := range append([]string{field}, c.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			for _, leaked := range []string{c.unquoted, fakeToken} {
				if strings.Contains(err.Error(), leaked) {
					t.Errorf("error = %q, want it free of %q", err, leaked)
				}
			}
		})
	}
}

func TestHolds(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		raw  string
		want bool
	}{
		"a bare ${VAR}":                     {raw: "${LORE_GATEWAY_URL}", want: true},
		"an env expansion":                  {raw: "${env:LORE_GATEWAY_URL}", want: true},
		"an expansion after an escaped one": {raw: "$${env:LORE_GATEWAY_URL}/${env:LORE_TENANT}", want: true},
		"an escaped expansion":              {raw: "$${env:LORE_GATEWAY_URL}", want: false},
		"plain text":                        {raw: "https://api.acme.dev/v1", want: false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := envx.Holds(c.raw); got != c.want {
				t.Errorf("Holds(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

func TestEscapeRoundTripsThroughExpansion(t *testing.T) {
	setFakeEnv(t)

	cases := map[string]string{
		"plain text":                         "sk-live-abc",
		"a bare open":                        "Pa${ss}word",
		"a whole env expansion":              "${env:LORE_ENVX_TOKEN}",
		"an escaped open already present":    "ghp_X$${Y}",
		"a dollar run ending in an open":     "$$${",
		"two opens back to back":             "${${",
		"a trailing dollar":                  "trailing $",
		"braces and dollars that never open": "{$}$",
	}

	for name, typed := range cases {
		t.Run(name, func(t *testing.T) {
			got, names, err := envx.ExpandNames(field, envx.Escape(typed))
			if err != nil {
				t.Fatalf("ExpandNames(Escape(%q)) error = %v, want the literal back", typed, err)
			}
			if got != typed || names != nil {
				t.Errorf("ExpandNames(Escape(%q)) = %q reading %v, want %q reading nothing", typed, got, names, typed)
			}
		})
	}
}
