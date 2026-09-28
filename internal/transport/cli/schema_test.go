package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInitWritesTheSchemaItsModelinePointsAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")

	res := runOn(t, sourceRegistry(t), nil, "", "init", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	firstLine, _, _ := strings.Cut(readConfigFile(t, path), "\n")
	target, found := strings.CutPrefix(firstLine, "# yaml-language-server: $schema=")
	if !found {
		t.Fatalf("first line = %q, want a yaml-language-server modeline", firstLine)
	}
	if want := "./team.schema.json"; target != want {
		t.Errorf("modeline points at %q, want %q beside the configuration", target, want)
	}
	if !strings.Contains(res.stdout, "wrote "+filepath.Join(dir, "team.schema.json")) {
		t.Errorf("stdout = %q, want it to name the schema written", res.stdout)
	}

	schema := readSchema(t, filepath.Join(dir, target))
	if got := useSuggestions(schema); !slices.Equal(got, []string{"forge", "tracker"}) {
		t.Errorf("sources[].use suggests %v, want the registered source plugins", got)
	}
}

func TestSchemaChecksTheInstalledPluginsTheConfigurationDeclares(t *testing.T) {
	path := installLinearSource(t)
	declared := readConfigFile(t, path) + "  - name: ghost\n    from: github.com/jdoe/lore-ghost@v0.1.0\n"
	if err := os.WriteFile(path, []byte(declared), 0o600); err != nil {
		t.Fatalf("declare an uninstalled plugin: %v", err)
	}

	res := runOn(t, sourceRegistry(t), nil, "", "schema", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "unchecked: plugins[ghost] — not installed") {
		t.Errorf("stdout = %q, want it to say why ghost's with: blocks go unchecked", res.stdout)
	}

	schema := readSchema(t, schemaPath(path))
	if got := useSuggestions(schema); !slices.Equal(got, []string{"forge", "tracker", "linear", "ghost"}) {
		t.Errorf("sources[].use suggests %v, want the compiled, installed and declared plugins", got)
	}
	if got := requiredWithKeys(t, schema, "linear"); !slices.Equal(got, []string{"team", "api_key"}) {
		t.Errorf("linear's with: requires %v, want its required field and, installed from outside the binary, its secret", got)
	}
}

func readSchema(t *testing.T, path string) map[string]any {
	t.Helper()

	var schema map[string]any
	if err := json.Unmarshal([]byte(readConfigFile(t, path)), &schema); err != nil {
		t.Fatalf("%s is not JSON: %v", path, err)
	}
	return schema
}

func sourceItem(schema map[string]any) map[string]any {
	sources := schema["properties"].(map[string]any)["sources"].(map[string]any)
	return sources["items"].(map[string]any)
}

func useSuggestions(schema map[string]any) []string {
	use := sourceItem(schema)["properties"].(map[string]any)["use"].(map[string]any)
	return stringList(use["anyOf"].([]any)[0].(map[string]any)["enum"])
}

func requiredWithKeys(t *testing.T, schema map[string]any, plugin string) []string {
	t.Helper()

	for _, rule := range sourceItem(schema)["allOf"].([]any) {
		rule := rule.(map[string]any)
		use := rule["if"].(map[string]any)["properties"].(map[string]any)["use"].(map[string]any)
		if use["const"] != plugin {
			continue
		}
		with := rule["then"].(map[string]any)["properties"].(map[string]any)["with"].(map[string]any)
		return stringList(with["required"])
	}
	t.Fatalf("the schema has no with: rule for %s", plugin)
	return nil
}

func stringList(list any) []string {
	var out []string
	for _, item := range list.([]any) {
		out = append(out, item.(string))
	}
	return out
}
