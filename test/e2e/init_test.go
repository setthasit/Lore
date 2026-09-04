package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/setthasit/Lore/internal/config"
)

// cli keeps its exit codes unexported; this is the one it returns on success.
const exitOK = 0

// The round trip is the scaffold's one hard contract: a file `lore init` writes
// and the next `lore` invocation rejects would strand a fresh workspace. It has
// to hold over the plugin set the real binary registers, not a hand-wired
// subset, which is why it runs here rather than beside the command.
func TestInitScaffoldDecodesForTheOfficialPluginSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lore.yaml")

	exitCode, stderr := runLore(t, "init", "--config", path)
	if exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", exitCode, stderr)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the written scaffold: %v", err)
	}
	scaffold := string(written)

	if _, err := config.Decode(strings.NewReader(scaffold)); err != nil {
		t.Fatalf("the scaffold does not decode: %v\n--- scaffold ---\n%s", err, scaffold)
	}
	var tree map[string]any
	if err := yaml.Unmarshal(written, &tree); err != nil {
		t.Fatalf("the scaffold is not valid YAML: %v", err)
	}
	assertNoSecretValues(t, scaffold)
	t.Logf("scaffold for the official plugin set:\n%s", scaffold)
}

// assertNoSecretValues holds the line no generated file may cross: a *_env key
// names a variable, and anything else that looks like a credential key would be
// holding the credential itself.
func assertNoSecretValues(t *testing.T, content string) {
	t.Helper()

	for _, line := range strings.Split(content, "\n") {
		key, _, assigns := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#")), ":")
		if !assigns {
			continue
		}
		for _, secretish := range []string{"token", "key", "secret", "password"} {
			if strings.HasSuffix(key, secretish) {
				t.Errorf("line %q assigns a secret directly; only *_env keys are allowed", line)
			}
		}
	}
}
