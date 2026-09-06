package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

func TestConfigReadRefusalsMatchAcrossCommands(t *testing.T) {
	reads := []struct {
		name  string
		stage func(t *testing.T, path string)
		code  int
		want  func(path string) string
	}{{
		name:  "missing",
		stage: func(*testing.T, string) {},
		code:  exitNotFound,
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
		code: exitInternal,
		want: func(path string) string { return "cannot read " + path },
	}, {
		name: "unparseable",
		stage: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("workspace: [unterminated\n"), 0o600); err != nil {
				t.Fatalf("stage an unparseable configuration: %v", err)
			}
		},
		code: exitBadRequest,
		want: func(path string) string { return "cannot parse " + path },
	}}

	commands := []struct {
		name string
		run  func(t *testing.T, path string) (int, string)
	}{{
		name: "plugin remove",
		run: func(t *testing.T, path string) (int, string) {
			newFakeReleases(t)
			res := runPluginDist(t, "plugin", "remove", "linear", "--config", path)
			return res.exitCode, res.stderr
		},
	}, {
		name: "source add",
		run: func(t *testing.T, path string) (int, string) {
			res := runPlugins(t, sourceRegistry(t), forgeAnswers, "source", "add", "forge", "--config", path)
			return res.exitCode, res.stderr
		},
	}}

	for _, read := range reads {
		for _, command := range commands {
			t.Run(read.name+" via "+command.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "lore.yaml")
				read.stage(t, path)

				code, stderr := command.run(t, path)
				if code != read.code {
					t.Fatalf("exit = %d, want %d (stderr %q)", code, read.code, stderr)
				}
				if want := read.want(path); !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr, want)
				}
			})
		}
	}
}

func TestWriteConfigRefusesADocumentThatNoLongerDecodes(t *testing.T) {
	path := writeConfigFile(t, seeded)
	const refusal = "the forge instance does not fit the configuration, which is unchanged"

	err := writeConfig(path, "workspace: myproject\nsources: [broken\n", refusal)
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
	if after := readConfigFile(t, path); after != seeded {
		t.Errorf("file =\n%s\nwant the original, untouched", after)
	}
}

func TestWriteConfigKeepsTheFileMode(t *testing.T) {
	path := writeConfigFile(t, seeded)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("stage the file mode: %v", err)
	}

	updated := seeded + "\n# a line the splice appended\n"
	if err := writeConfig(path, updated, "unused"); err != nil {
		t.Fatalf("write a document that decodes: %v", err)
	}

	if after := readConfigFile(t, path); after != updated {
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
