package cli

import (
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/registry"
)

// The warnings are computed by the registry from what the built connectors say
// they ingest, so a command's job is only to put them on the right stream.
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
