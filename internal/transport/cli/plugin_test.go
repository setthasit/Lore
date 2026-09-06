package cli

import (
	"bytes"
	"testing"

	"github.com/setthasit/Lore/internal/registry"
	lore "github.com/setthasit/Lore/sdk"
)

func TestRenderPluginsOutput(t *testing.T) {
	entries := []registry.Entry{
		{Manifest: lore.Manifest{Name: "git", Kind: lore.KindCode, Summary: "Commits and diffs"}, Origin: "built-in"},
		{
			Manifest: lore.Manifest{
				Name:         "linear-issues",
				Kind:         lore.KindProvider,
				Summary:      "Linear issues",
				Capabilities: lore.Capabilities{Embed: true},
			},
			Origin: "external /opt/x",
		},
		{Manifest: lore.Manifest{Name: "ünïcode", Kind: lore.KindCode, Summary: "Multibyte name"}, Origin: "built-in"},
		{Manifest: lore.Manifest{Name: "ünïcode-wïdest", Kind: lore.KindCode, Summary: "Widest is multibyte"}, Origin: "built-in"},
		{Manifest: lore.Manifest{Name: "日本語", Kind: lore.KindCode, Summary: "Double-width runes count as one"}, Origin: "built-in"},
	}

	tests := []struct {
		name      string
		entries   []registry.Entry
		externals []externalRow
		want      string
	}{
		{
			name: "no plugins",
			want: "no plugins are registered — this build can ingest nothing\n",
		},
		{
			name:    "columns pad to the widest cell in runes, not bytes and not display width",
			entries: entries,
			want: "NAME            KIND              ORIGIN           SUMMARY\n" +
				"git             code              built-in         Commits and diffs\n" +
				"linear-issues   provider (embed)  external /opt/x  Linear issues\n" +
				"ünïcode         code              built-in         Multibyte name\n" +
				"ünïcode-wïdest  code              built-in         Widest is multibyte\n" +
				"日本語             code              built-in         Double-width runes count as one\n",
		},
		{
			name:      "declared externals align with the table's name column",
			entries:   entries,
			externals: []externalRow{{name: "crm", from: "github.com/acme/lore-crm", state: "not installed — run: lore plugin install crm"}},
			want: "NAME            KIND              ORIGIN           SUMMARY\n" +
				"git             code              built-in         Commits and diffs\n" +
				"linear-issues   provider (embed)  external /opt/x  Linear issues\n" +
				"ünïcode         code              built-in         Multibyte name\n" +
				"ünïcode-wïdest  code              built-in         Widest is multibyte\n" +
				"日本語             code              built-in         Double-width runes count as one\n" +
				"\n" +
				"crm             declared from github.com/acme/lore-crm\n" +
				"                not installed — run: lore plugin install crm\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			renderPlugins(&out, test.entries, test.externals)
			if got := out.String(); got != test.want {
				t.Errorf("renderPlugins() =\n%q\nwant\n%q", got, test.want)
			}
		})
	}
}
