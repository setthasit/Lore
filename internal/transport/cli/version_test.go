package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	mock_services "github.com/setthasit/Lore/internal/mocks/services"
)

const (
	versionConfigured = "openai/text-embedding-3-small/1536"
	versionIndexed    = "ollama/nomic-embed-text/768"
)

func mockIdentity(t *testing.T) (*Runtime, *mock_services.MockStatusService) {
	t.Helper()

	status := mock_services.NewMockStatusService(gomock.NewController(t))
	return &Runtime{
		Config: &config.Config{Workspace: "demo", IndexPath: "/tmp/demo.db"},
		Status: status,
	}, status
}

// The resolver fails the way a missing lore.yaml or an unopenable index fails.
func runVersionWithBrokenWorkspace(t *testing.T, err error) result {
	t.Helper()

	var out, errOut bytes.Buffer
	res := result{}

	resolve := func(context.Context, string, ...fx.Option) (*Runtime, func() error, error) {
		return nil, nil, err
	}

	root := newRootCommand(resolve)
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"--version"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		res.exitCode = report(&errOut, err)
	}
	res.stdout, res.stderr = out.String(), errOut.String()
	return res
}

func TestVersionReportsTheBuildStampAndTheWorkspaceIdentity(t *testing.T) {
	rt, status := mockIdentity(t)
	status.EXPECT().EmbedderIdentity(gomock.Any()).
		Return(entities.EmbedderIdentity{Configured: versionConfigured, Indexed: versionConfigured}, nil)

	res := run(t, rt, "--version")

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	for _, want := range []string{
		"lore ",
		"build:",
		"workspace: demo — /tmp/demo.db",
		"embedder:  " + versionConfigured,
		"index:     " + versionConfigured,
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", res.stdout, want)
		}
	}
	if !res.released {
		t.Error("the workspace was not released")
	}
}

// A never-synced index is the normal state right after `lore init`: reporting it
// as a mismatch would send the reader to a pointless re-embed.
func TestVersionSaysWhenTheIndexHoldsNoVectorsYet(t *testing.T) {
	rt, status := mockIdentity(t)
	status.EXPECT().EmbedderIdentity(gomock.Any()).
		Return(entities.EmbedderIdentity{Configured: versionConfigured}, nil)

	res := run(t, rt, "--version")

	if !strings.Contains(res.stdout, "index:     no vectors yet — run `lore sync`") {
		t.Errorf("stdout = %q, want the never-synced index reported", res.stdout)
	}
}

func TestVersionFlagsAnEmbedderMismatchWithItsRemedy(t *testing.T) {
	rt, status := mockIdentity(t)
	status.EXPECT().EmbedderIdentity(gomock.Any()).
		Return(entities.EmbedderIdentity{Configured: versionConfigured, Indexed: versionIndexed}, nil)

	res := run(t, rt, "--version")

	if !strings.Contains(res.stdout, versionIndexed+" — mismatch; run `lore sync --reembed`") {
		t.Errorf("stdout = %q, want the mismatch and its remedy", res.stdout)
	}
}

// --version is the first command a bug report runs, so a broken workspace is
// reported on stdout and still exits zero.
func TestVersionSurvivesAnUnresolvableWorkspace(t *testing.T) {
	res := runVersionWithBrokenWorkspace(t,
		fxLikeWrap(internalerror.NewBadRequestError("cannot read ./lore.yaml", nil)))

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "lore ") || !strings.Contains(res.stdout, "build:") {
		t.Errorf("stdout = %q, want the build stamp printed anyway", res.stdout)
	}
	if !strings.Contains(res.stdout, "workspace: unavailable — ") {
		t.Errorf("stdout = %q, want the workspace reported as unavailable", res.stdout)
	}
}

func TestVersionReportsAnUnreadableEmbedderIdentity(t *testing.T) {
	rt, status := mockIdentity(t)
	status.EXPECT().EmbedderIdentity(gomock.Any()).
		Return(entities.EmbedderIdentity{}, internalerror.NewInternalError("reading the index's embedder identity failed", nil))

	res := run(t, rt, "--version")

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "embedder:  unavailable — ") {
		t.Errorf("stdout = %q, want the identity read reported as unavailable", res.stdout)
	}
}

// An unstamped binary still has to identify itself: every field falls back.
func TestStampNeverPrintsAnEmptyField(t *testing.T) {
	s := stamp()

	if s.Version == "" || s.Commit == "" || s.Date == "" || s.GoVersion == "" || s.Platform == "" {
		t.Errorf("stamp = %+v, want every field filled", s)
	}
	if !strings.Contains(s.Platform, "/") {
		t.Errorf("platform = %q, want GOOS/GOARCH", s.Platform)
	}
}
