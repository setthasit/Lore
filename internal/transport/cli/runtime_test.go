package cli

import (
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/entities"
)

func configWithRemote(remote string, ingested ...string) *config.Config {
	return &config.Config{
		Workspace: "myproject",
		Sources:   config.Sources{GitHub: &config.GitHubSource{TokenEnv: "LORE_GITHUB_TOKEN", Repos: ingested}},
		Repos:     []config.Repo{{Path: "/home/dev/myproject", Remote: remote}},
	}
}

func TestStartupWarningsReachStderrOnly(t *testing.T) {
	rt := mockStatus(t, entities.IndexStats{}, nil)
	rt.Config = configWithRemote("github:acme/nope", "acme/yes")

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

func TestAMappedRemoteWarnsAboutNothing(t *testing.T) {
	rt := mockStatus(t, entities.IndexStats{}, nil)
	rt.Config = configWithRemote("github:acme/yes", "acme/yes")

	res := run(t, rt, "status")

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if res.stderr != "" {
		t.Errorf("stderr = %q, want empty", res.stderr)
	}
}
