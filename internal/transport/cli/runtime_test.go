package cli

import (
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/secrets"
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
	sink.Record("LORE_ACME_TOKEN (sources[acme].with.token_env)", "t-short")

	res, unnoticed := status(sink), status(nil)

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	want := "lore: secrets shorter than 8 characters are not scrubbed: LORE_ACME_TOKEN (sources[acme].with.token_env)\n"
	if res.stderr != want {
		t.Errorf("stderr = %q, want exactly %q", res.stderr, want)
	}
	if res.stdout != unnoticed.stdout {
		t.Errorf("stdout = %q, want it unchanged by the notice: %q", res.stdout, unnoticed.stdout)
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
