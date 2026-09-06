package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

const editable = `workspace: myproject

# Sources say what to INGEST: one item per instance, in sync order.
sources:
  - use: forge                             # the starter instance
    with:
      token_env: LORE_FORGE_TOKEN
`

func TestReadFileClassifiesEveryRefusal(t *testing.T) {
	cases := []struct {
		name  string
		stage func(t *testing.T, path string)
		kind  internalerror.Kind
		want  func(path string) string
	}{{
		name:  "missing",
		stage: func(*testing.T, string) {},
		kind:  internalerror.KindNotFound,
		want: func(path string) string {
			return "no configuration at " + path + " — run `lore init` to create one"
		},
	}, {
		// A directory is unreadable even as root; a 0o000 bit is not.
		name: "unreadable",
		stage: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatalf("stage an unreadable configuration: %v", err)
			}
		},
		kind: internalerror.KindInternal,
		want: func(path string) string { return "cannot read " + path },
	}, {
		name: "unparseable",
		stage: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("workspace: [unterminated\n"), 0o600); err != nil {
				t.Fatalf("stage an unparseable configuration: %v", err)
			}
		},
		kind: internalerror.KindBadRequest,
		want: func(path string) string { return "cannot parse " + path },
	}, {
		name: "unknown key",
		stage: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("workspace: myproject\nnosuch: 1\n"), 0o600); err != nil {
				t.Fatalf("stage a configuration with an unknown key: %v", err)
			}
		},
		kind: internalerror.KindBadRequest,
		want: func(path string) string { return "cannot parse " + path },
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lore.yaml")
			tc.stage(t, path)

			_, _, err := ReadFile(path)
			var classified *internalerror.Error
			if !errors.As(err, &classified) {
				t.Fatalf("error = %v, want a classified refusal", err)
			}
			if classified.Kind != tc.kind {
				t.Errorf("kind = %v, want %v", classified.Kind, tc.kind)
			}
			if want := tc.want(path); classified.Message != want {
				t.Errorf("message = %q, want %q", classified.Message, want)
			}
		})
	}
}

// The text is returned verbatim because every edit is a splice into it: a
// reflowed document would lose the comments the operator wrote.
func TestReadFileReturnsTheFileVerbatim(t *testing.T) {
	path := writeEditable(t, editable)

	text, cfg, err := ReadFile(path)
	if err != nil {
		t.Fatalf("read a configuration that decodes: %v", err)
	}
	if text != editable {
		t.Errorf("text =\n%s\nwant\n%s", text, editable)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0].Use != "forge" {
		t.Errorf("sources = %+v, want the one declared instance", cfg.Sources)
	}
}

func TestWriteFileRefusesADocumentThatNoLongerDecodes(t *testing.T) {
	path := writeEditable(t, editable)
	const refusal = "the forge instance does not fit the configuration, which is unchanged"

	err := WriteFile(path, "workspace: myproject\nsources: [broken\n", refusal)
	if err == nil {
		t.Fatal("a spliced document that does not decode was written")
	}
	var classified *internalerror.Error
	if !errors.As(err, &classified) {
		t.Fatalf("error = %v, want a classified refusal", err)
	}
	if classified.Kind != internalerror.KindInternal {
		t.Errorf("kind = %v, want %v", classified.Kind, internalerror.KindInternal)
	}
	if classified.Message != refusal {
		t.Errorf("message = %q, want the caller's own sentence %q", classified.Message, refusal)
	}
	if after := readEditable(t, path); after != editable {
		t.Errorf("file =\n%s\nwant the original, untouched", after)
	}
}

func TestWriteFileKeepsTheFileMode(t *testing.T) {
	path := writeEditable(t, editable)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("stage the file mode: %v", err)
	}

	updated := editable + "\n# a line the splice appended\n"
	if err := WriteFile(path, updated, "unused"); err != nil {
		t.Fatalf("write a document that decodes: %v", err)
	}

	if after := readEditable(t, path); after != updated {
		t.Errorf("file =\n%s\nwant\n%s", after, updated)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the configuration: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want the mode the file already carried", info.Mode().Perm())
	}
}

func writeEditable(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "lore.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed a configuration: %v", err)
	}
	return path
}

func readEditable(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
