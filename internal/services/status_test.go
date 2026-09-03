package services_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	mock_embedder "github.com/setthasit/Lore/internal/mocks/embedder"
	mock_repositories "github.com/setthasit/Lore/internal/mocks/repositories"
	"github.com/setthasit/Lore/internal/services"
)

const statusIdentity = "openai/text-embedding-3-small/1536"

var errStatusStore = errors.New("index is unreadable")

func newStatusFixture(t *testing.T) (*mock_repositories.MockIndexStore, *mock_embedder.MockEmbedder, services.StatusService) {
	t.Helper()

	ctrl := gomock.NewController(t)
	store := mock_repositories.NewMockIndexStore(ctrl)
	emb := mock_embedder.NewMockEmbedder(ctrl)
	return store, emb, services.NewStatusService(store, emb)
}

func TestStatusReportsWhatTheIndexHolds(t *testing.T) {
	store, _, svc := newStatusFixture(t)

	at := time.Date(2025, time.March, 12, 9, 30, 0, 0, time.UTC)
	want := entities.IndexStats{
		Documents: 1284,
		Chunks:    9613,
		Edges:     431,
		Cursors:   []entities.CursorAge{{Connector: "github", UpdatedAt: at}},
		Lease:     &entities.LeaseState{Holder: "host-1/4242", AcquiredAt: at, HeartbeatAt: at},
	}
	store.EXPECT().Stats(gomock.Any()).Return(want, nil)

	got, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Documents != want.Documents || got.Chunks != want.Chunks || got.Edges != want.Edges {
		t.Errorf("counts = %d, %d, %d; want %d, %d, %d",
			got.Documents, got.Chunks, got.Edges, want.Documents, want.Chunks, want.Edges)
	}
	if len(got.Cursors) != 1 || got.Cursors[0] != want.Cursors[0] {
		t.Errorf("cursors = %+v, want %+v", got.Cursors, want.Cursors)
	}
	if got.Lease == nil || *got.Lease != *want.Lease {
		t.Errorf("lease = %+v, want %+v", got.Lease, want.Lease)
	}
}

func TestStatusClassifiesAStoreFailure(t *testing.T) {
	store, _, svc := newStatusFixture(t)
	store.EXPECT().Stats(gomock.Any()).Return(entities.IndexStats{}, errStatusStore)

	got, err := svc.Status(context.Background())
	if err == nil {
		t.Fatal("Status: want an error")
	}
	if kind := internalerror.KindOf(err); kind != internalerror.KindInternal {
		t.Errorf("kind = %s, want %s", kind, internalerror.KindInternal)
	}
	if !errors.Is(err, errStatusStore) {
		t.Errorf("error %v does not wrap the store's failure", err)
	}
	if got.Documents != 0 || got.Chunks != 0 || got.Cursors != nil || got.Lease != nil {
		t.Errorf("stats = %+v, want the zero report on failure", got)
	}
}

func TestEmbedderIdentityReportsBothSides(t *testing.T) {
	store, emb, svc := newStatusFixture(t)
	emb.EXPECT().Identity().Return(statusIdentity)
	store.EXPECT().Meta(gomock.Any(), "embedder_identity").Return(statusIdentity, nil)

	got, err := svc.EmbedderIdentity(context.Background())
	if err != nil {
		t.Fatalf("EmbedderIdentity: %v", err)
	}
	if got.Configured != statusIdentity || got.Indexed != statusIdentity {
		t.Errorf("identity = %+v, want both sides %q", got, statusIdentity)
	}
}

// A workspace that has never synced is configured but not yet committed to a
// vector space: the empty half is the signal, not an error.
func TestEmbedderIdentityLeavesTheIndexedSideEmptyBeforeTheFirstSync(t *testing.T) {
	store, emb, svc := newStatusFixture(t)
	emb.EXPECT().Identity().Return(statusIdentity)
	store.EXPECT().Meta(gomock.Any(), "embedder_identity").Return("", nil)

	got, err := svc.EmbedderIdentity(context.Background())
	if err != nil {
		t.Fatalf("EmbedderIdentity: %v", err)
	}
	if got.Configured != statusIdentity || got.Indexed != "" {
		t.Errorf("identity = %+v, want %q configured and nothing indexed", got, statusIdentity)
	}
}

func TestEmbedderIdentityClassifiesAStoreFailure(t *testing.T) {
	store, emb, svc := newStatusFixture(t)
	emb.EXPECT().Identity().Return(statusIdentity).AnyTimes()
	store.EXPECT().Meta(gomock.Any(), "embedder_identity").Return("", errStatusStore)

	got, err := svc.EmbedderIdentity(context.Background())
	if err == nil {
		t.Fatal("EmbedderIdentity: want an error")
	}
	if kind := internalerror.KindOf(err); kind != internalerror.KindInternal {
		t.Errorf("kind = %s, want %s", kind, internalerror.KindInternal)
	}
	if !errors.Is(err, errStatusStore) {
		t.Errorf("error %v does not wrap the store's failure", err)
	}
	if got != (entities.EmbedderIdentity{}) {
		t.Errorf("identity = %+v, want the zero report on failure", got)
	}
}
