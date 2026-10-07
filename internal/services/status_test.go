package services_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/repositories/sqlite"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/sdk"
)

const statusIdentity = "openai/text-embedding-3-small/1536"

var statusCheckpoint = time.Date(2025, time.March, 12, 9, 30, 0, 0, time.UTC)

func newStatusFixture(t *testing.T, declared entities.DeclaredWorkspace) (*sqlite.Store, services.StatusService) {
	t.Helper()

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "workspace.db"), 3, sqlite.WithClock(func() time.Time { return statusCheckpoint }))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store, services.NewStatusService(store, statusIdentity, declared)
}

func TestStatusReportsWhatTheIndexHolds(t *testing.T) {
	store, svc := newStatusFixture(t, entities.DeclaredWorkspace{Sources: []string{"github"}})
	ctx := context.Background()
	first := lore.NewDocID("github", lore.DocTypePR, "one")
	second := lore.NewDocID("github", lore.DocTypeIssue, "two")
	if err := store.UpsertDocuments(ctx, []lore.Document{
		{ID: first, Source: "github", Type: lore.DocTypePR},
		{ID: second, Source: "github", Type: lore.DocTypeIssue},
	}); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	if err := store.ReplaceChunks(ctx, first, []entities.Chunk{
		{DocID: first, Ordinal: 0, Text: "first", Source: "github", DocType: lore.DocTypePR, Embedding: []float32{1, 0, 0}},
		{DocID: first, Ordinal: 1, Text: "second", Source: "github", DocType: lore.DocTypePR, Embedding: []float32{0, 1, 0}},
		{DocID: first, Ordinal: 2, Text: "third", Source: "github", DocType: lore.DocTypePR, Embedding: []float32{0, 0, 1}},
	}); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}
	if err := store.UpsertEdges(ctx, []entities.Edge{{Src: first, Dst: second, Kind: entities.EdgeKindPRClosesIssue, Confidence: 1}}); err != nil {
		t.Fatalf("UpsertEdges: %v", err)
	}
	if err := store.SetCursor(ctx, "github", nil); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	if ok, err := store.TryAcquireLease(ctx, "host-1/4242"); err != nil || !ok {
		t.Fatalf("TryAcquireLease = %v, %v, want true, nil", ok, err)
	}

	at := statusCheckpoint
	want := entities.IndexStats{
		Documents: 2,
		Chunks:    3,
		Edges:     1,
		Sources:   []entities.SourceState{{ID: "github", Configured: true, Documents: 2, LastCheckpoint: at}},
		Lease:     &entities.LeaseState{Holder: "host-1/4242", AcquiredAt: at, HeartbeatAt: at},
	}

	got, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Documents != want.Documents || got.Chunks != want.Chunks || got.Edges != want.Edges {
		t.Errorf("counts = %d, %d, %d; want %d, %d, %d",
			got.Documents, got.Chunks, got.Edges, want.Documents, want.Chunks, want.Edges)
	}
	if !slices.Equal(got.Sources, want.Sources) {
		t.Errorf("sources = %+v, want %+v", got.Sources, want.Sources)
	}
	if got.Lease == nil || *got.Lease != *want.Lease {
		t.Errorf("lease = %+v, want %+v", got.Lease, want.Lease)
	}
}

func TestStatusListsAConfiguredSourceThatNeverSynced(t *testing.T) {
	_, svc := newStatusFixture(t, entities.DeclaredWorkspace{Sources: []string{"notion", "jira", "github"}})

	got, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := []entities.SourceState{
		{ID: "github", Configured: true},
		{ID: "jira", Configured: true},
		{ID: "notion", Configured: true},
	}
	if !slices.Equal(got.Sources, want) {
		t.Errorf("sources = %+v, want %+v", got.Sources, want)
	}
	if got.Documents != 0 || got.Chunks != 0 || got.Edges != 0 || got.Lease != nil {
		t.Errorf("stats = %+v, want zero counts and no lease", got)
	}
}

func TestStatusKeepsAnIndexedSourceNoLongerConfigured(t *testing.T) {
	store, svc := newStatusFixture(t, entities.DeclaredWorkspace{Sources: []string{"jira"}})
	ctx := context.Background()
	if err := store.UpsertDocuments(ctx, []lore.Document{
		{ID: lore.NewDocID("github", lore.DocTypePR, "one"), Source: "github", Type: lore.DocTypePR},
		{ID: lore.NewDocID("notion", lore.DocTypePage, "one"), Source: "notion", Type: lore.DocTypePage},
	}); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	for _, id := range []string{"github", "gitlab"} {
		if err := store.SetCursor(ctx, id, nil); err != nil {
			t.Fatalf("SetCursor(%s): %v", id, err)
		}
	}

	got, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := []entities.SourceState{
		{ID: "github", Documents: 1, LastCheckpoint: statusCheckpoint},
		{ID: "gitlab", LastCheckpoint: statusCheckpoint},
		{ID: "jira", Configured: true},
		{ID: "notion", Documents: 1},
	}
	if !slices.Equal(got.Sources, want) {
		t.Errorf("sources = %+v, want %+v", got.Sources, want)
	}
	checkpointed := 0
	for _, source := range got.Sources {
		if !source.LastCheckpoint.IsZero() {
			checkpointed++
		}
	}
	if got.Documents != 2 || checkpointed != 2 {
		t.Errorf("stats = %+v, want two documents and two checkpointed sources", got)
	}
}

func TestStatusCarriesClonesAsDeclared(t *testing.T) {
	_, svc := newStatusFixture(t, entities.DeclaredWorkspace{Clones: []entities.DeclaredClone{
		{Name: "github:acme/myproject", Synced: true},
		{Name: "github:acme/archive", Synced: false},
		{Name: "repos[2] (no remote)", Synced: false},
	}})

	got, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := []entities.CloneState{
		{Name: "github:acme/myproject", Synced: true},
		{Name: "github:acme/archive", Synced: false},
		{Name: "repos[2] (no remote)", Synced: false},
	}
	if !slices.Equal(got.Clones, want) {
		t.Errorf("clones = %+v, want %+v", got.Clones, want)
	}
}

func TestStatusLabelsLocalNamesWithoutChangingIdentity(t *testing.T) {
	for _, name := range []string{
		"/Users/operator/private", "/tmp/clone", "~/private", "~operator/private", ".", "..",
		"./private", "../private", "relative/private", `C:\private\clone`, "C:/private/clone",
		`c:private\clone`, "C:private", `\\server\share\clone`, `\private\clone`,
		"file:///Users/operator/private", "FILE://server/share/clone", "file:relative/clone",
	} {
		t.Run(name, func(t *testing.T) {
			declared := entities.DeclaredWorkspace{
				Sources: []string{name},
				Clones:  []entities.DeclaredClone{{Name: name, Synced: true}},
			}
			store, svc := newStatusFixture(t, declared)
			ctx := context.Background()
			if err := store.UpsertDocuments(ctx, []lore.Document{{ID: "fixture:page:one", Source: name, Type: lore.DocTypePage}}); err != nil {
				t.Fatalf("UpsertDocuments: %v", err)
			}
			if err := store.SetCursor(ctx, name, nil); err != nil {
				t.Fatalf("SetCursor: %v", err)
			}
			want := entities.IndexStats{
				Documents: 1,
				Sources:   []entities.SourceState{{ID: "sources[0] (local id)", Configured: true, Documents: 1, LastCheckpoint: statusCheckpoint}},
				Clones:    []entities.CloneState{{Name: "repos[0] (local remote)", Synced: true}},
			}
			for range 2 {
				got, err := svc.Status(ctx)
				if err != nil {
					t.Fatalf("Status: %v", err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("status = %+v, want %+v", got, want)
				}
			}
			raw, err := store.Stats(ctx)
			if err != nil {
				t.Fatalf("Stats: %v", err)
			}
			if !slices.Equal(raw.Sources, []entities.SourceState{{ID: name, Documents: 1, LastCheckpoint: statusCheckpoint}}) {
				t.Errorf("stored source changed: %+v", raw.Sources)
			}
			if declared.Sources[0] != name || declared.Clones[0].Name != name || !declared.Clones[0].Synced {
				t.Errorf("declared inputs changed: %+v", declared)
			}
		})
	}
}

func TestStatusKeepsOrdinaryCloneNamesAndMatchedCustomForgeGrammar(t *testing.T) {
	for _, name := range []string{"jira", "jira-acme", "github:acme/myproject", "custom.example:group/subgroup/project", "git+custom:namespace/name", "git@host:group/project", "g:team/repo", "C:private/clone"} {
		t.Run(name, func(t *testing.T) {
			_, svc := newStatusFixture(t, entities.DeclaredWorkspace{Sources: []string{"jira-acme"}, Clones: []entities.DeclaredClone{{Name: name, Synced: true}}})
			got, err := svc.Status(context.Background())
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if !slices.Equal(got.Sources, []entities.SourceState{{ID: "jira-acme", Configured: true}}) || !slices.Equal(got.Clones, []entities.CloneState{{Name: name, Synced: true}}) {
				t.Errorf("ordinary name changed: %+v", got)
			}
		})
	}
}

func TestStatusLabelsIndexedColonPathIDsWithoutForgeExemption(t *testing.T) {
	for _, id := range []string{"legacy:private/clone", "g:team/repo", "custom.example:group/subgroup/project"} {
		t.Run(id, func(t *testing.T) {
			store, svc := newStatusFixture(t, entities.DeclaredWorkspace{})
			ctx := context.Background()
			if err := store.UpsertDocuments(ctx, []lore.Document{{ID: "fixture:page:one", Source: id, Type: lore.DocTypePage}}); err != nil {
				t.Fatalf("UpsertDocuments: %v", err)
			}
			if err := store.SetCursor(ctx, id, nil); err != nil {
				t.Fatalf("SetCursor: %v", err)
			}
			got, err := svc.Status(ctx)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			want := []entities.SourceState{{ID: "sources[0] (local id)", Documents: 1, LastCheckpoint: statusCheckpoint}}
			if got.Documents != 1 || !slices.Equal(got.Sources, want) {
				t.Errorf("status = %+v, want the orphan labelled without losing its count or checkpoint", got)
			}
			raw, err := store.Stats(ctx)
			if err != nil {
				t.Fatalf("Stats: %v", err)
			}
			if !slices.Equal(raw.Sources, []entities.SourceState{{ID: id, Documents: 1, LastCheckpoint: statusCheckpoint}}) {
				t.Errorf("stored identity changed: %+v", raw.Sources)
			}
		})
	}
}

func TestStatusLabelsAreDistinctFromLiteralLabelsAndIndexedOrphans(t *testing.T) {
	store, svc := newStatusFixture(t, entities.DeclaredWorkspace{
		Sources: []string{"/private", "sources[0] (local id)"},
		Clones: []entities.DeclaredClone{
			{Name: "/private"}, {Name: "repos[0] (local remote)"}, {Name: `C:\private`},
		},
	})
	ctx := context.Background()
	if err := store.SetCursor(ctx, "/orphan", nil); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	got, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	wantSources := []entities.SourceState{
		{ID: "sources[1] (local id)", LastCheckpoint: statusCheckpoint},
		{ID: "sources[2] (local id)", Configured: true},
		{ID: "sources[0] (local id)", Configured: true},
	}
	if !slices.Equal(got.Sources, wantSources) {
		t.Errorf("sources = %+v, want %+v", got.Sources, wantSources)
	}
	wantClones := []entities.CloneState{{Name: "repos[1] (local remote)"}, {Name: "repos[0] (local remote)"}, {Name: "repos[2] (local remote)"}}
	if !slices.Equal(got.Clones, wantClones) {
		t.Errorf("clones = %+v, want %+v", got.Clones, wantClones)
	}
}

func TestStatusDoesNotTurnGitURLsIntoForgeReferences(t *testing.T) {
	for _, remote := range []string{"https://git.example/group/repo.git", "ssh://git@git.example/group/repo.git"} {
		t.Run(remote, func(t *testing.T) {
			_, svc := newStatusFixture(t, entities.DeclaredWorkspace{Clones: []entities.DeclaredClone{{Name: remote}}})
			got, err := svc.Status(context.Background())
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if !slices.Equal(got.Clones, []entities.CloneState{{Name: "repos[0] (local remote)"}}) {
				t.Errorf("clone = %+v, want an opaque label, not a forged reference", got.Clones)
			}
		})
	}
}

func TestStatusClassifiesAStoreFailure(t *testing.T) {
	_, svc := newStatusFixture(t, entities.DeclaredWorkspace{
		Sources: []string{"jira"},
		Clones:  []entities.DeclaredClone{{Name: "github:acme/myproject", Synced: true}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := svc.Status(ctx)
	if err == nil {
		t.Fatal("Status: want an error")
	}
	if kind := internalerror.KindOf(err); kind != internalerror.KindInternal {
		t.Errorf("kind = %s, want %s", kind, internalerror.KindInternal)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap the store's failure", err)
	}
	if !reflect.DeepEqual(got, entities.IndexStats{}) {
		t.Errorf("stats = %+v, want the zero report on failure", got)
	}
}

func TestEmbedderIdentityReportsBothSides(t *testing.T) {
	store, svc := newStatusFixture(t, entities.DeclaredWorkspace{})
	if err := store.SetMeta(context.Background(), "embedder_identity", statusIdentity); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	got, err := svc.EmbedderIdentity(context.Background())
	if err != nil {
		t.Fatalf("EmbedderIdentity: %v", err)
	}
	if got.Configured != statusIdentity || got.Indexed != statusIdentity {
		t.Errorf("identity = %+v, want both sides %q", got, statusIdentity)
	}
}

func TestEmbedderIdentityLeavesTheIndexedSideEmptyBeforeTheFirstSync(t *testing.T) {
	_, svc := newStatusFixture(t, entities.DeclaredWorkspace{})

	got, err := svc.EmbedderIdentity(context.Background())
	if err != nil {
		t.Fatalf("EmbedderIdentity: %v", err)
	}
	if got.Configured != statusIdentity || got.Indexed != "" {
		t.Errorf("identity = %+v, want %q configured and nothing indexed", got, statusIdentity)
	}
}

func TestEmbedderIdentityClassifiesAStoreFailure(t *testing.T) {
	_, svc := newStatusFixture(t, entities.DeclaredWorkspace{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := svc.EmbedderIdentity(ctx)
	if err == nil {
		t.Fatal("EmbedderIdentity: want an error")
	}
	if kind := internalerror.KindOf(err); kind != internalerror.KindInternal {
		t.Errorf("kind = %s, want %s", kind, internalerror.KindInternal)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap the store's failure", err)
	}
	if got != (entities.EmbedderIdentity{}) {
		t.Errorf("identity = %+v, want the zero report on failure", got)
	}
}
