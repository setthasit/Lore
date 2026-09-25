package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/envx"
	"github.com/setthasit/Lore/plugins"
	"github.com/setthasit/Lore/sdk"
)

// cli keeps its exit codes unexported; this is the one it returns on success.
const exitOK = 0

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
	assertSecretsReferenceTheEnvironment(t, scaffold, plugins.Official()...)
	t.Logf("scaffold for the official plugin set:\n%s", scaffold)
}

func assertSecretsReferenceTheEnvironment(t *testing.T, content string, declaring ...lore.Plugin) {
	t.Helper()

	keys := map[string]bool{}
	for _, plugin := range declaring {
		for _, secret := range plugin.Manifest().Secrets {
			keys[secret.Key] = true
		}
	}
	secrets := 0
	for _, line := range strings.Split(content, "\n") {
		key, value, assigns := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#")), ":")
		if !assigns || !keys[key] {
			continue
		}
		secrets++
		value, _, _ = strings.Cut(value, " #")
		name, expands := strings.CutPrefix(strings.TrimSpace(value), "${env:")
		name, closed := strings.CutSuffix(name, "}")
		if !expands || !closed || !envx.ValidName(name) {
			t.Errorf("line %q holds a credential; a secret must read ${env:VAR}", line)
		}
	}
	if secrets == 0 {
		t.Errorf("no secret line to check in\n%s", content)
	}
}
