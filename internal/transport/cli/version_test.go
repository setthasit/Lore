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
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/sdk"
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

func runVersionWithBrokenWorkspace(t *testing.T, err error) result {
	t.Helper()

	var out, errOut bytes.Buffer
	res := result{}

	resolve := func(context.Context, string, ...fx.Option) (*Runtime, func() error, error) {
		return nil, nil, err
	}

	reg := registry.New(lore.Host{}, nil)
	root := newRootCommand(resolve, reg)
	root.SetArgs([]string{"--version"})

	res.exitCode = execute(context.Background(), root, reg.Sink(), &out, &errOut)
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

func TestVersionPrintsAHostileManifestFieldDocInert(t *testing.T) {
	fieldDoc := "the base URL\x1b[2K\nworkspace: demo — /tmp/demo.db\rread\u202egnitirw"
	res := runVersionWithBrokenWorkspace(t, fxLikeWrap(internalerror.NewBadRequestError(
		"providers[openai].with.api_base must be set — "+fieldDoc, nil)))

	want := "workspace: unavailable — providers[openai].with.api_base must be set — " +
		`the base URL\x1b[2K\nworkspace: demo — /tmp/demo.db\rread\u202egnitirw` + "\n"
	if !strings.Contains(res.stdout, want) {
		t.Errorf("stdout = %q, want it to contain %q", res.stdout, want)
	}
	assertInert(t, res.stdout)
}

func TestVersionPrintsAHostileWorkspaceNameAndIndexPathInert(t *testing.T) {
	rt, status := mockIdentity(t)
	rt.Config = &config.Config{Workspace: "demo\x1b[31m", IndexPath: "/tmp/\u202edemo.db"}
	status.EXPECT().EmbedderIdentity(gomock.Any()).
		Return(entities.EmbedderIdentity{Configured: versionConfigured, Indexed: versionConfigured}, nil)

	res := run(t, rt, "--version")

	want := `workspace: demo\x1b[31m — /tmp/\u202edemo.db` + "\n"
	if !strings.Contains(res.stdout, want) {
		t.Errorf("stdout = %q, want it to contain %q", res.stdout, want)
	}
	assertInert(t, res.stdout)
}

func TestVersionPrintsAHostileEmbedderIdentityInert(t *testing.T) {
	rt, status := mockIdentity(t)
	identity := "openai/text-\x1b[2Kembedding\u202e-3-small/1536"
	status.EXPECT().EmbedderIdentity(gomock.Any()).
		Return(entities.EmbedderIdentity{Configured: identity, Indexed: identity}, nil)

	res := run(t, rt, "--version")

	inert := `openai/text-\x1b[2Kembedding\u202e-3-small/1536`
	for _, want := range []string{"embedder:  " + inert + "\n", "index:     " + inert + "\n"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", res.stdout, want)
		}
	}
	assertInert(t, res.stdout)
}

func TestVersionPrintsAHostileMismatchedIndexIdentityInert(t *testing.T) {
	rt, status := mockIdentity(t)
	status.EXPECT().EmbedderIdentity(gomock.Any()).Return(entities.EmbedderIdentity{
		Configured: versionConfigured,
		Indexed:    "ollama/nomic\r\nindex:     " + versionConfigured,
	}, nil)

	res := run(t, rt, "--version")

	want := "index:     ollama/nomic\\r\\nindex:     " + versionConfigured +
		" — mismatch; run `lore sync --reembed`\n"
	if !strings.Contains(res.stdout, want) {
		t.Errorf("stdout = %q, want it to contain %q", res.stdout, want)
	}
	assertInert(t, res.stdout)
}

func TestStampNeverPrintsAnEmptyField(t *testing.T) {
	s := stamp()

	if s.Version == "" || s.Commit == "" || s.Date == "" || s.GoVersion == "" || s.Platform == "" {
		t.Errorf("stamp = %+v, want every field filled", s)
	}
	if !strings.Contains(s.Platform, "/") {
		t.Errorf("platform = %q, want GOOS/GOARCH", s.Platform)
	}
}
