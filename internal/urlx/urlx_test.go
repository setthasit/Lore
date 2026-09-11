package urlx_test

import (
	"net/url"
	"testing"

	"github.com/setthasit/Lore/internal/urlx"
)

func TestRedact(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		raw  string
		want string
	}{
		"userinfo and the whole query go": {
			raw: "https://svcaccount:fake-not-a-real-token@artifacts.acme.dev:8443" +
				"/lore/acme-crm/v2.0.1.tar.gz?sig=fake-signature&expires=1",
			want: "https://artifacts.acme.dev:8443/lore/acme-crm/v2.0.1.tar.gz",
		},
		"a username without a password goes too": {
			raw:  "https://fake-not-a-real-token@artifacts.acme.dev/v2.0.1.tar.gz",
			want: "https://artifacts.acme.dev/v2.0.1.tar.gz",
		},
		"a URL with nothing to strip is unchanged": {
			raw:  "https://artifacts.acme.dev/lore/acme-crm/v2.0.1.tar.gz",
			want: "https://artifacts.acme.dev/lore/acme-crm/v2.0.1.tar.gz",
		},
		"a token in a path segment survives": {
			raw:  "https://artifacts.acme.dev/fake-not-a-real-token/v2.0.1.tar.gz",
			want: "https://artifacts.acme.dev/fake-not-a-real-token/v2.0.1.tar.gz",
		},
		"a fragment survives": {
			raw:  "https://artifacts.acme.dev/v2.0.1.tar.gz#part",
			want: "https://artifacts.acme.dev/v2.0.1.tar.gz#part",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			parsed, err := url.Parse(c.raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := urlx.Redact(parsed); got != c.want {
				t.Errorf("Redact() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRedactLeavesTheCallersURLIntact(t *testing.T) {
	t.Parallel()

	const raw = "https://svcaccount:fake-not-a-real-token@artifacts.acme.dev/v2.0.1.tar.gz?sig=fake-signature"
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	urlx.Redact(parsed)

	if parsed.String() != raw {
		t.Errorf("caller URL = %q, want it untouched as %q", parsed, raw)
	}
}

func TestRedactIfUserinfo(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		raw  string
		want string
	}{
		"an uppercase scheme is still a credentialed URL": {
			raw:  "HTTPS://svcaccount:fake-not-a-real-token@artifacts.acme.dev/v2.0.1.tar.gz?sig=fake-signature",
			want: "https://artifacts.acme.dev/v2.0.1.tar.gz",
		},
		"a scheme no coordinate accepts is still redacted": {
			raw:  "git+https://svcaccount:fake-not-a-real-token@acme.dev/lore.git?sig=fake-signature",
			want: "git+https://acme.dev/lore.git",
		},
		"a URL with no userinfo keeps even its query": {
			raw:  "https://artifacts.acme.dev/v2.0.1.tar.gz?release=77",
			want: "https://artifacts.acme.dev/v2.0.1.tar.gz?release=77",
		},
		"a relative path is untouched": {
			raw:  "./bin/lore-linear",
			want: "./bin/lore-linear",
		},
		"a windows path keeps its drive letter": {
			raw:  `C:\bin\lore-linear`,
			want: `C:\bin\lore-linear`,
		},
		"a github coordinate is untouched": {
			raw:  "github.com/jdoe/lore-linear@v0.3.1",
			want: "github.com/jdoe/lore-linear@v0.3.1",
		},
		"a string no parser accepts is untouched": {
			raw:  "http:// acme.dev/lore?sig=fake-signature",
			want: "http:// acme.dev/lore?sig=fake-signature",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := urlx.RedactIfUserinfo(c.raw); got != c.want {
				t.Errorf("RedactIfUserinfo(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}
