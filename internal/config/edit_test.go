package config

import (
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

type keySpec struct {
	key       string
	fields    []Field
	item      func(indent string) string
	existing  func(indent string) string
	bare      string
	inline    string
	multiline []struct{ shape, item string }
	count     func(cfg *Config) int
}

var sourcesSpec = keySpec{
	key:    "sources",
	fields: []Field{{Key: "use", Value: "tracker"}},
	item:   func(indent string) string { return indent + "- use: tracker\n" },
	existing: func(indent string) string {
		return indent + "- use: forge\n" + indent + "  with:\n" + indent + "    token_env: LORE_FORGE_TOKEN\n" +
			indent + "    repos:\n" + indent + "      - acme/app\n"
	},
	bare:   "  -\n    use: forge\n",
	inline: "[{use: forge, with: {token_env: LORE_FORGE_TOKEN}}]",
	multiline: []struct{ shape, item string }{
		{"literal", "  - use: forge\n    with:\n      note: |\n        one\n        two\n"},
		{"folded", "  - use: forge\n    with:\n      note: >\n        one\n        two\n"},
		{"double-quoted", "  - use: forge\n    with:\n      note: \"one\n        two\"\n"},
		{"plain", "  - use: forge\n    with:\n      note: one\n        two\n"},
	},
	count: func(cfg *Config) int { return len(cfg.Sources) },
}

var pluginsSpec = keySpec{
	key:    "plugins",
	fields: []Field{{Key: "name", Value: "linear"}, {Key: "from", Value: "m@v1"}},
	item:   func(indent string) string { return indent + "- name: linear\n" + indent + "  from: m@v1\n" },
	existing: func(indent string) string {
		return indent + "- name: other\n" + indent + "  from: o@v1\n"
	},
	bare:   "  -\n    name: other\n    from: o@v1\n",
	inline: "[{name: other, from: o@v1}]",
	multiline: []struct{ shape, item string }{
		{"literal", "  - name: other\n    from: |\n      one\n      two\n"},
		{"folded", "  - name: other\n    from: >\n      one\n      two\n"},
		{"double-quoted", "  - name: other\n    from: \"one\n      two\"\n"},
		{"plain", "  - name: other\n    from: one\n      two\n"},
	},
	count: func(cfg *Config) int { return len(cfg.Plugins) },
}

var editKeys = []keySpec{sourcesSpec, pluginsSpec}

type appendCase struct {
	name    string
	content string
	want    string
	items   int
	refused bool
}

func appendCases(k keySpec) []appendCase {
	cases := []appendCase{{
		name:    "the key is absent",
		content: "workspace: x\n",
		want:    "workspace: x\n" + k.key + ":\n" + k.item("  "),
		items:   1,
	}, {
		name:    "the key carries nothing",
		content: "workspace: x\n" + k.key + ":\nrepos: []\n",
		want:    "workspace: x\n" + k.key + ":\n" + k.item("  ") + "repos: []\n",
		items:   1,
	}, {
		name:    "the key carries nothing but a comment",
		content: "workspace: x\n" + k.key + ": # later\n",
		want:    "workspace: x\n" + k.key + ": # later\n" + k.item("  "),
		items:   1,
	}, {
		name:    "an empty flow sequence is reopened",
		content: "workspace: x\n" + k.key + ": []\nrepos: []\n",
		want:    "workspace: x\n" + k.key + ":\n" + k.item("  ") + "repos: []\n",
		items:   1,
	}, {
		name:    "reopening keeps the trailing comment",
		content: "workspace: x\n" + k.key + ": []                # nothing yet\nrepos: []\n",
		want:    "workspace: x\n" + k.key + ": # nothing yet\n" + k.item("  ") + "repos: []\n",
		items:   1,
	}, {
		name:    "an empty flow sequence spanning lines is refused",
		content: "workspace: x\n" + k.key + ": [\n]\nrepos: []\n",
		refused: true,
	}, {
		name:    "an inline flow sequence of items is refused",
		content: "workspace: x\n" + k.key + ": " + k.inline + "\n",
		refused: true,
	}, {
		name:    "an inline scalar is refused",
		content: "workspace: x\n" + k.key + ": nonsense\n",
		refused: true,
	}, {
		name:    "a mapping under the key is refused",
		content: "workspace: x\n" + k.key + ":\n  mapped: 1\n",
		refused: true,
	}, {
		name:    "items at two spaces",
		content: k.key + ":\n" + k.existing("  ") + "repos: []\n",
		want:    k.key + ":\n" + k.existing("  ") + k.item("  ") + "repos: []\n",
		items:   2,
	}, {
		name:    "items at four spaces",
		content: k.key + ":\n" + k.existing("    ") + "repos: []\n",
		want:    k.key + ":\n" + k.existing("    ") + k.item("    ") + "repos: []\n",
		items:   2,
	}, {
		name:    "items at column zero",
		content: k.key + ":\n" + k.existing("") + "repos: []\n",
		want:    k.key + ":\n" + k.existing("") + k.item("") + "repos: []\n",
		items:   2,
	}, {
		name:    "a blank line and a comment below the block stay below the new item",
		content: k.key + ":\n" + k.existing("  ") + "\n# introduces the repos\nrepos: []\n",
		want:    k.key + ":\n" + k.existing("  ") + k.item("  ") + "\n# introduces the repos\nrepos: []\n",
		items:   2,
	}, {
		name:    "an indented comment ending the block stays below the new item",
		content: k.key + ":\n" + k.existing("  ") + "  # introduces the repos\nrepos: []\n",
		want:    k.key + ":\n" + k.existing("  ") + k.item("  ") + "  # introduces the repos\nrepos: []\n",
		items:   2,
	}, {
		name:    "the file has no trailing newline",
		content: strings.TrimSuffix(k.key+":\n"+k.existing("  "), "\n"),
		want:    k.key + ":\n" + k.existing("  ") + k.item("  "),
		items:   2,
	}, {
		name:    "the file uses CRLF",
		content: crlf(k.key + ":\n" + k.existing("  ") + "repos: []\n"),
		want:    crlf(k.key+":\n"+k.existing("  ")) + k.item("  ") + crlf("repos: []\n"),
		items:   2,
	}, {
		name:    "an item whose dash sits on its own line",
		content: k.key + ":\n" + k.bare + "repos: []\n",
		want:    k.key + ":\n" + k.bare + k.item("  ") + "repos: []\n",
		items:   2,
	}}

	for _, shape := range k.multiline {
		cases = append(cases, appendCase{
			name:    "the last item ends in a " + shape.shape + " scalar",
			content: k.key + ":\n" + shape.item + "repos: []\n",
			want:    k.key + ":\n" + shape.item + k.item("  ") + "repos: []\n",
			items:   2,
		})
	}
	return cases
}

func TestAppendItem(t *testing.T) {
	for _, k := range editKeys {
		for _, tc := range appendCases(k) {
			t.Run(k.key+"/"+tc.name, func(t *testing.T) {
				block, err := FindBlock(tc.content, k.key)
				if tc.refused {
					assertRefusedAsInline(t, err)
					return
				}
				if err != nil {
					t.Fatalf("find the block: %v", err)
				}

				got, err := block.AppendItem(k.fields)
				if err != nil {
					t.Fatalf("append an item: %v", err)
				}
				if got != tc.want {
					t.Errorf("text =\n%q\nwant\n%q", got, tc.want)
				}
				assertItemCount(t, k, got, tc.items)
			})
		}
	}
}

func TestAppendItemQuotesANameYAMLWouldMisread(t *testing.T) {
	tests := []struct{ name, want string }{
		{"no", `  - name: "no"`},
		{"null", `  - name: "null"`},
		{"x: y", `  - name: 'x: y'`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			block, err := FindBlock("workspace: x\n", "plugins")
			if err != nil {
				t.Fatalf("find the block: %v", err)
			}

			got, err := block.AppendItem([]Field{{Key: "name", Value: tc.name}, {Key: "from", Value: "m@v1"}})
			if err != nil {
				t.Fatalf("append an item: %v", err)
			}
			if !strings.Contains(got, tc.want+"\n") {
				t.Errorf("text =\n%q\nwant it to render %q", got, tc.want)
			}
			cfg := decodeText(t, got)
			if len(cfg.Plugins) != 1 || cfg.Plugins[0].Name != tc.name {
				t.Fatalf("plugins = %+v, want the one named %q\n%s", cfg.Plugins, tc.name, got)
			}
		})
	}
}

func TestRemove(t *testing.T) {
	tests := []struct {
		name    string
		content string
		remove  string
		want    string
		items   int
	}{{
		name:    "the only item takes the key with it",
		content: "workspace: x\n\nplugins:\n  - name: linear\n    from: m@v1\n",
		remove:  "linear",
		want:    "workspace: x\n\n",
		items:   0,
	}, {
		name: "a middle item leaves the comment below it",
		content: "plugins:\n  - name: a\n    from: a@v1\n  - name: linear\n    from: m@v1\n" +
			"  # introduces the last one\n  - name: z\n    from: z@v1\nrepos: []\n",
		remove: "linear",
		want: "plugins:\n  - name: a\n    from: a@v1\n" +
			"  # introduces the last one\n  - name: z\n    from: z@v1\nrepos: []\n",
		items: 2,
	}, {
		name:    "an item spanning a folded scalar goes whole",
		content: "plugins:\n  - name: linear\n    from: >\n      one\n      two\n  - name: z\n    from: z@v1\n",
		remove:  "linear",
		want:    "plugins:\n  - name: z\n    from: z@v1\n",
		items:   1,
	}, {
		name:    "the file has no trailing newline",
		content: "plugins:\n  - name: linear\n    from: m@v1\n  - name: z\n    from: z@v1",
		remove:  "linear",
		want:    "plugins:\n  - name: z\n    from: z@v1",
		items:   1,
	}, {
		name:    "an item whose dash sits on its own line",
		content: "plugins:\n  -\n    name: linear\n    from: m@v1\n  - name: z\n    from: z@v1\n",
		remove:  "linear",
		want:    "plugins:\n  - name: z\n    from: z@v1\n",
		items:   1,
	}, {
		name: "an item carrying a nested sequence goes whole",
		content: "plugins:\n  - name: linear\n    from: m@v1\n    tags:\n      - one\n      - two\n" +
			"  - name: z\n    from: z@v1\n",
		remove: "linear",
		want:   "plugins:\n  - name: z\n    from: z@v1\n",
		items:  1,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			block, err := FindBlock(tc.content, "plugins")
			if err != nil {
				t.Fatalf("find the block: %v", err)
			}
			item, found := block.Find(tc.remove)
			if !found {
				t.Fatalf("the block does not declare %q", tc.remove)
			}

			got := block.Remove(item)
			if got != tc.want {
				t.Errorf("text =\n%q\nwant\n%q", got, tc.want)
			}
			assertItemCount(t, pluginsSpec, got, tc.items)
		})
	}
}

func TestFindReportsAnUndeclaredName(t *testing.T) {
	block, err := FindBlock("plugins:\n  - name: other\n    from: o@v1\n", "plugins")
	if err != nil {
		t.Fatalf("find the block: %v", err)
	}
	if item, found := block.Find("linear"); found {
		t.Errorf("found %+v, want no item", item)
	}
}

func TestSetField(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		want       string
		rewritable bool
	}{{
		name:       "the value keeps its comment",
		content:    "plugins:\n  - name: linear\n    from: o@v1 # keep me\n",
		want:       "plugins:\n  - name: linear\n    from: m@v2 # keep me\n",
		rewritable: true,
	}, {
		name:       "the value keeps the item's indentation",
		content:    "plugins:\n    - name: linear\n      from: o@v1\nrepos: []\n",
		want:       "plugins:\n    - name: linear\n      from: m@v2\nrepos: []\n",
		rewritable: true,
	}, {
		name:       "the file has no trailing newline",
		content:    "plugins:\n  - name: linear\n    from: o@v1",
		want:       "plugins:\n  - name: linear\n    from: m@v2",
		rewritable: true,
	}, {
		name:    "the value sits on its own line",
		content: "plugins:\n  - name: linear\n    from:\n      o@v1\n",
	}, {
		name:    "the field is absent",
		content: "plugins:\n  - name: linear\n",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			block, err := FindBlock(tc.content, "plugins")
			if err != nil {
				t.Fatalf("find the block: %v", err)
			}
			item, found := block.Find("linear")
			if !found {
				t.Fatal("the block does not declare linear")
			}

			got, rewritten := block.SetField(item, "from", "m@v2")
			if rewritten != tc.rewritable {
				t.Fatalf("rewritten = %v, want %v (text %q)", rewritten, tc.rewritable, got)
			}
			if !tc.rewritable {
				return
			}
			if got != tc.want {
				t.Errorf("text =\n%q\nwant\n%q", got, tc.want)
			}
			if cfg := decodeText(t, got); len(cfg.Plugins) != 1 || cfg.Plugins[0].From != "m@v2" {
				t.Errorf("plugins = %+v, want the rewritten coordinate", cfg.Plugins)
			}
		})
	}
}

func TestAppendItemRendersNestedFields(t *testing.T) {
	block, err := FindBlock("workspace: x\n", "sources")
	if err != nil {
		t.Fatalf("find the block: %v", err)
	}

	got, err := block.AppendItem([]Field{
		{Key: "id", Value: "tracker-eu"},
		{Key: "use", Value: "tracker"},
		{Key: "with", Value: []Field{
			{Key: "base_url", Value: "https://tracker.example"},
			{Key: "batch", Value: 10},
			{Key: "projects", Value: []string{"PROJ", "INFRA"}},
		}},
	})
	if err != nil {
		t.Fatalf("append an item: %v", err)
	}

	want := "workspace: x\nsources:\n  - id: tracker-eu\n    use: tracker\n    with:\n" +
		"      base_url: https://tracker.example\n      batch: 10\n      projects:\n" +
		"        - PROJ\n        - INFRA\n"
	if got != want {
		t.Errorf("text =\n%q\nwant\n%q", got, want)
	}
	assertItemCount(t, sourcesSpec, got, 1)
}

func crlf(text string) string {
	return strings.ReplaceAll(text, "\n", "\r\n")
}

func assertRefusedAsInline(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("the inline value was accepted")
	}
	if !internalerror.IsPrecondition(err) {
		t.Errorf("kind = %v, want %v", internalerror.KindOf(err), internalerror.KindPrecondition)
	}
	if !strings.Contains(internalerror.MessageOf(err), "inline value") {
		t.Errorf("message = %q, want it to name the inline value", internalerror.MessageOf(err))
	}
}

func assertItemCount(t *testing.T, k keySpec, text string, want int) {
	t.Helper()

	if got := k.count(decodeText(t, text)); got != want {
		t.Errorf("%s = %d items, want %d\n%s", k.key, got, want, text)
	}
}

func decodeText(t *testing.T, text string) *Config {
	t.Helper()

	cfg, err := Decode(strings.NewReader(text))
	if err != nil {
		t.Fatalf("the result does not decode: %v\n%s", err, text)
	}
	return cfg
}
