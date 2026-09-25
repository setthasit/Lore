package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

const editable = `workspace: myproject

# Sources say what to INGEST: one item per instance, in sync order.
sources:
  - use: forge                             # the starter instance
    with:
      token: ${env:LORE_FORGE_TOKEN}
`

// Every byte of an excerpt can escape to \xNN, so the widest quoted detail is what an all-NUL excerpt renders to.
var maxExcerpt = len(internalerror.Excerpt(strings.Repeat("\x00", 1<<10)))

func TestReadFileClassifiesEveryRefusal(t *testing.T) {
	cases := []struct {
		name   string
		stage  func(t *testing.T, path string)
		kind   internalerror.Kind
		want   func(path string) string
		detail []string
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
		kind:   internalerror.KindBadRequest,
		want:   func(path string) string { return "cannot parse " + path },
		detail: []string{"line 1"},
	}, {
		name: "unknown key",
		stage: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("workspace: myproject\nnosuch: 1\n"), 0o600); err != nil {
				t.Fatalf("stage a configuration with an unknown key: %v", err)
			}
		},
		kind:   internalerror.KindBadRequest,
		want:   func(path string) string { return "cannot parse " + path },
		detail: []string{"line 2", "nosuch"},
	}, {
		name: "control bytes in a key",
		stage: func(t *testing.T, path string) {
			body := "workspace: myproject\n\"\\e]0;pwned\\a" + strings.Repeat("x", 200) + "\": 1\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("stage a configuration whose key carries control bytes: %v", err)
			}
		},
		kind:   internalerror.KindBadRequest,
		want:   func(path string) string { return "cannot parse " + path },
		detail: []string{"line 2", `\x1b]0;pwned\a`},
	}, {
		name: "a key of control bytes reaches the widest escape",
		stage: func(t *testing.T, path string) {
			body := "workspace: myproject\n\"" + strings.Repeat(`\0`, 200) + "\": 1\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("stage a configuration whose key is all control bytes: %v", err)
			}
		},
		kind:   internalerror.KindBadRequest,
		want:   func(path string) string { return "cannot parse " + path },
		detail: []string{"line 2", `\x00`},
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
			if i := strings.IndexFunc(classified.Message, unicode.IsControl); i >= 0 {
				t.Errorf("message = %q carries a control byte at %d", classified.Message, i)
			}
			if bound := len(tc.want(path)) + len(": ") + maxExcerpt; len(classified.Message) > bound {
				t.Errorf("message is %d bytes, want at most %d", len(classified.Message), bound)
			}
			if len(tc.detail) == 0 {
				if want := tc.want(path); classified.Message != want {
					t.Errorf("message = %q, want %q", classified.Message, want)
				}
				return
			}
			if want := tc.want(path) + ": "; !strings.HasPrefix(classified.Message, want) {
				t.Errorf("message = %q, want it to start with %q", classified.Message, want)
			}
			for _, detail := range tc.detail {
				if !strings.Contains(classified.Message, detail) {
					t.Errorf("message = %q, want it to carry %q", classified.Message, detail)
				}
			}
		})
	}
}

// The text is returned verbatim because every edit is a splice into it: a reflowed document would lose the comments the operator wrote.
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

	err := WriteFile(path, Splice{From: editable, To: "workspace: myproject\nsources: [broken\n"}, refusal)
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
	if err := WriteFile(path, Splice{From: editable, To: updated}, "unused"); err != nil {
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

func TestWriteFileRefusesAFileThatChangedSinceItWasRead(t *testing.T) {
	cases := []struct {
		name    string
		current string
	}{{
		name:    "the file was edited in place",
		current: strings.Replace(editable, "myproject", "myproj3ct", 1),
	}, {
		name:    "the file grew past what was read",
		current: editable + "repos: []\n",
	}}

	const draft = editable + "query:\n  top_k: 5\n"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEditable(t, tc.current)

			err := WriteFile(path, Splice{From: editable, To: draft}, "unused")
			var classified *internalerror.Error
			if !errors.As(err, &classified) {
				t.Fatalf("error = %v, want a classified refusal", err)
			}
			if classified.Kind != internalerror.KindPrecondition {
				t.Errorf("kind = %v, want %v", classified.Kind, internalerror.KindPrecondition)
			}
			want := path + " changed since it was read — re-run the command; the newer file is unchanged"
			if classified.Message != want {
				t.Errorf("message = %q, want %q", classified.Message, want)
			}
			if after := readEditable(t, path); after != tc.current {
				t.Errorf("file =\n%s\nwant the newer file, untouched\n%s", after, tc.current)
			}
		})
	}
}

func TestWriteFileRefusesAPathThatIsNoLongerARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lore.yaml")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("stage a path that is not a regular file: %v", err)
	}

	err := WriteFile(path, Splice{From: editable, To: editable + "repos: []\n"}, "unused")
	var classified *internalerror.Error
	if !errors.As(err, &classified) {
		t.Fatalf("error = %v, want a classified refusal", err)
	}
	if classified.Kind != internalerror.KindPrecondition {
		t.Errorf("kind = %v, want %v", classified.Kind, internalerror.KindPrecondition)
	}
	want := path + " is not a regular file — re-run the command; nothing was written"
	if classified.Message != want {
		t.Errorf("message = %q, want %q", classified.Message, want)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Errorf("path = %v (error %v), want the directory left in place", info, err)
	}
}

func TestWriteFileReportsAFileThatVanished(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lore.yaml")

	err := WriteFile(path, Splice{From: editable, To: editable + "repos: []\n"}, "unused")
	var classified *internalerror.Error
	if !errors.As(err, &classified) {
		t.Fatalf("error = %v, want a classified refusal", err)
	}
	if classified.Kind != internalerror.KindNotFound {
		t.Errorf("kind = %v, want %v", classified.Kind, internalerror.KindNotFound)
	}
	want := "no configuration at " + path + " — run `lore init` to create one"
	if classified.Message != want {
		t.Errorf("message = %q, want %q", classified.Message, want)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat = %v, want the file still absent", err)
	}
}

func TestWriteFileRefusesAConfigurationItCannotReRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lore.yaml")
	if err := os.Symlink(path, path); err != nil {
		t.Fatalf("stage a configuration that cannot be opened: %v", err)
	}

	err := WriteFile(path, Splice{From: editable, To: editable + "repos: []\n"}, "unused")
	var classified *internalerror.Error
	if !errors.As(err, &classified) {
		t.Fatalf("error = %v, want a classified refusal", err)
	}
	if classified.Kind != internalerror.KindPrecondition {
		t.Errorf("kind = %v, want %v", classified.Kind, internalerror.KindPrecondition)
	}
	want := "cannot re-read the configuration to confirm it is unchanged: open " + path + ": " +
		syscall.ELOOP.Error() + " — re-run the command; nothing was written"
	if classified.Message != want {
		t.Errorf("message = %q, want %q", classified.Message, want)
	}
}

func TestWriteFileReplacesASymlinkWithARegularFile(t *testing.T) {
	target := writeEditable(t, editable)
	link := filepath.Join(filepath.Dir(target), "linked.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("stage a symlinked configuration: %v", err)
	}

	updated := editable + "repos: []\n"
	if err := WriteFile(link, Splice{From: editable, To: updated}, "unused"); err != nil {
		t.Fatalf("write a configuration reached through a symlink: %v", err)
	}
	if after := readEditable(t, link); after != updated {
		t.Errorf("file =\n%s\nwant\n%s", after, updated)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("stat the written path: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("mode = %v, want the link replaced by a regular file", info.Mode())
	}
	if after := readEditable(t, target); after != editable {
		t.Errorf("target =\n%s\nwant the file the link pointed at, untouched\n%s", after, editable)
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
