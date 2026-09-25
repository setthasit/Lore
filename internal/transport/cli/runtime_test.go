package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/secrets"
	"github.com/setthasit/Lore/sdk"
)

func TestStartupWarningsReachStderrOnly(t *testing.T) {
	rt := mockStatus(t, entities.IndexStats{}, nil)
	rt.Config = &config.Config{Workspace: "myproject"}
	rt.Warnings = registry.Warnings{
		"repos path /home/dev/myproject has remote github:acme/nope, which names no configured source repo",
	}

	res := run(t, rt, "status")

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if got := strings.Count(res.stderr, "lore: warning: "); got != 1 {
		t.Fatalf("stderr = %q, want exactly one warning line", res.stderr)
	}
	if !strings.Contains(res.stderr, "github:acme/nope") {
		t.Errorf("stderr = %q, want it to name the unmapped remote", res.stderr)
	}
	if strings.Contains(res.stdout, "warning") || strings.Contains(res.stdout, "acme/nope") {
		t.Errorf("stdout = %q, want it free of warnings", res.stdout)
	}
}

func TestStartupWarningsReachStderrInert(t *testing.T) {
	rt := mockStatus(t, entities.IndexStats{}, nil)
	rt.Config = &config.Config{Workspace: "myproject"}
	rt.Warnings = registry.Warnings{
		"repos path /home/dev/myproject has remote github:acme/" + clearScreen + "lore, which names no configured source repo",
	}

	res := run(t, rt, "status")

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	assertInert(t, res.stderr)
	if !strings.Contains(res.stderr, "github:acme/"+clearScreenInert+"lore") {
		t.Errorf("stderr = %q, want the remote named with its escape shown", res.stderr)
	}
}

func TestShortSecretNoticeReachesStderrOnly(t *testing.T) {
	status := func(sink *secrets.Sink) result {
		rt := mockStatus(t, entities.IndexStats{}, nil)
		rt.Config = &config.Config{Workspace: "myproject"}
		rt.Sink = sink
		return run(t, rt, "status")
	}
	sink := &secrets.Sink{}
	sink.Record("sources[acme].with.token", "t-short")

	res, unnoticed := status(sink), status(nil)

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	want := "lore: secrets shorter than 8 characters are not scrubbed: sources[acme].with.token\n"
	if res.stderr != want {
		t.Errorf("stderr = %q, want exactly %q", res.stderr, want)
	}
	if res.stdout != unnoticed.stdout {
		t.Errorf("stdout = %q, want it unchanged by the notice: %q", res.stdout, unnoticed.stdout)
	}
}

func serveMCPWithForgeToken(t *testing.T, token string) result {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	_ = writer.Close()
	restore := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = restore
		_ = reader.Close()
	})

	reg := registry.New(lore.Host{}, &secrets.Sink{})
	if _, _, err := reg.Prepare(forgePlugin().Manifest(), registry.Instance{
		Use:   "forge",
		Field: "sources[forge]",
		With:  map[string]any{"repos": []any{"acme/lore"}, "token": token},
	}, registry.OriginBuiltin); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return runOn(t, reg, &Runtime{Sink: reg.Sink()}, "", "mcp")
}

func TestLiteralSecretNoticeReachesStderrOnly(t *testing.T) {
	const credential = "fake-token-123456789"

	res := serveMCPWithForgeToken(t, credential)

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if got := strings.Count(res.stderr, "\n"); got != 1 {
		t.Fatalf("stderr = %q, want exactly one line", res.stderr)
	}
	for _, want := range []string{"sources[forge].with.token", "${env:VAR}"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", res.stderr, want)
		}
	}
	if strings.Contains(res.stderr, credential) {
		t.Errorf("stderr = %q, want the credential left out", res.stderr)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want it empty: it carries the JSON-RPC stream", res.stdout)
	}
}

func TestExpandedSecretsLeaveStderrSilent(t *testing.T) {
	t.Setenv("LORE_FORGE_TOKEN", "fake-token-123456789")

	res := serveMCPWithForgeToken(t, "${env:LORE_FORGE_TOKEN}")

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if res.stderr != "" {
		t.Errorf("stderr = %q, want empty", res.stderr)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
	}
}

func TestNoWarningsMeansASilentStderr(t *testing.T) {
	rt := mockStatus(t, entities.IndexStats{}, nil)
	rt.Config = &config.Config{Workspace: "myproject"}

	res := run(t, rt, "status")

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if res.stderr != "" {
		t.Errorf("stderr = %q, want empty", res.stderr)
	}
}
