package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/plugindist"
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

func TestPluginListReportsATamperedCacheAsTampered(t *testing.T) {
	fake := newFakeReleases(t)
	publishPlugin(t, fake, "v0.3.1", pluginStub(t), pluginScript)
	path := writeConfigFile(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1")+
		"  - name: crm\n    from: github.com/acme/lore-crm@v2.0.1\n")

	if res := run(t, nil, "plugin", "install", "linear", "--config", path); res.exitCode != exitOK {
		t.Fatalf("install: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	tamperCachedBinary(t)

	res := runOn(t, stubRegistry(t, forgePlugin()), nil, "", "plugin", "list", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	for _, want := range []string{
		"digest mismatch", "lore plugin install linear",
		"not installed — run: lore plugin install crm",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout %q does not mention %q", res.stdout, want)
		}
	}
	if strings.Contains(res.stdout, "not installed — run: lore plugin install linear") {
		t.Errorf("stdout %q reports the tampered cache as not installed", res.stdout)
	}
}

func TestPluginListReportsAWorkspaceThatDoesNotParse(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		lock     string
		wantFile string
	}{
		{
			name:     "a lore.yaml that does not parse",
			body:     "workspace: myproject\nplugins:\n  - name: [linear\n",
			wantFile: "lore.yaml",
		},
		{
			name:     "a lockfile that does not parse",
			body:     declaredConfig("github.com/jdoe/lore-linear@v0.3.1"),
			lock:     "plugins: [linear\n",
			wantFile: plugindist.LockFileName,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfigFile(t, test.body)
			if test.lock != "" {
				lock := filepath.Join(filepath.Dir(path), plugindist.LockFileName)
				if err := os.WriteFile(lock, []byte(test.lock), 0o600); err != nil {
					t.Fatalf("seed the lockfile: %v", err)
				}
			}

			res := runOn(t, stubRegistry(t, forgePlugin()), nil, "", "plugin", "list", "--config", path)
			if res.exitCode != exitBadRequest {
				t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
			}
			if want := "cannot parse " + filepath.Join(filepath.Dir(path), test.wantFile); !strings.Contains(res.stderr, want) {
				t.Errorf("stderr = %q, want it to name %q", res.stderr, want)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q, want no table for a workspace that cannot be read", res.stdout)
			}
		})
	}
}

func TestPluginListReportsAConfigurationItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows ignores mode 0000, so the file stays readable")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode 0000, so the file stays readable")
	}

	path := writeConfigFile(t, declaredConfig("github.com/jdoe/lore-linear@v0.3.1"))
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("make the configuration unreadable: %v", err)
	}

	res := runOn(t, stubRegistry(t, forgePlugin()), nil, "", "plugin", "list", "--config", path)
	if res.exitCode != exitInternal {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitInternal, res.stderr)
	}
	if want := "cannot read " + path; !strings.Contains(res.stderr, want) {
		t.Errorf("stderr = %q, want it to name %q", res.stderr, want)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want no table for a workspace that cannot be read", res.stdout)
	}
}

func TestPluginListWithoutAConfigurationListsThisBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lore.yaml")

	res := runOn(t, stubRegistry(t, forgePlugin()), nil, "", "plugin", "list", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	for _, want := range []string{"forge", "builtin", "a stub forge source"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout = %q, want it to list %q", res.stdout, want)
		}
	}
}
