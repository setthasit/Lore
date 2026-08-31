package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"lore/internal/connectors/embedder"
	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/repositories"
)

const metaKeyEmbedderIdentity = "embedder_identity"

type SyncOptions struct {
	// Wipes chunks, resets every cursor, and re-reads sources from the beginning.
	Reembed bool
}

type SyncOrchestrator interface {
	// Cursors advance only past durably stored documents, so a failed round is
	// consistent up to its last committed batch and re-running resumes without gaps.
	Sync(ctx context.Context, opts SyncOptions) error
}

type syncOrchestrator struct {
	store      repositories.IndexStore
	connectors []entities.Connector
	chunker    Chunker
	emb        embedder.Embedder
	links      LinkResolver
	holder     string
}

var _ SyncOrchestrator = (*syncOrchestrator)(nil)

func NewSyncOrchestrator(
	store repositories.IndexStore,
	connectors []entities.Connector,
	chunker Chunker,
	emb embedder.Embedder,
	links LinkResolver,
) SyncOrchestrator {
	return &syncOrchestrator{
		store:      store,
		connectors: connectors,
		chunker:    chunker,
		emb:        emb,
		links:      links,
		holder:     leaseHolder(),
	}
}

func leaseHolder() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}

	return host + "/" + strconv.Itoa(os.Getpid())
}

func (s *syncOrchestrator) Sync(ctx context.Context, opts SyncOptions) error {
	acquired, err := s.store.TryAcquireLease(ctx, s.holder)
	if err != nil {
		return internalerror.NewInternalError("could not take the workspace sync lease", err)
	}
	if !acquired {
		return internalerror.NewPreconditionError(
			"another sync holds the workspace lease — a scheduled round or another process is already writing this index; retry later, or wait out the 60s lease TTL if that holder crashed",
			nil)
	}
	defer func() { _ = s.store.ReleaseLease(context.WithoutCancel(ctx), s.holder) }()

	if err := s.reconcileIdentity(ctx, opts); err != nil {
		return err
	}

	for _, conn := range s.connectors {
		if err := s.syncConnector(ctx, conn); err != nil {
			return err
		}
	}

	return s.links.LinkPending(ctx)
}

func (s *syncOrchestrator) reconcileIdentity(ctx context.Context, opts SyncOptions) error {
	want := s.emb.Identity()

	stored, err := s.store.Meta(ctx, metaKeyEmbedderIdentity)
	if err != nil {
		return internalerror.NewInternalError("could not read the index's embedder identity", err)
	}

	if opts.Reembed {
		return s.reembed(ctx, want)
	}

	switch stored {
	case want:
		return nil
	case "":
		if err := s.store.SetMeta(ctx, metaKeyEmbedderIdentity, want); err != nil {
			return internalerror.NewInternalError(
				fmt.Sprintf("could not record the embedder identity %q", want), err)
		}

		return nil
	default:
		return internalerror.NewPreconditionError(fmt.Sprintf(
			"embedder identity mismatch: this index was built with %q but the workspace is now configured for %q — vectors from one embedder are meaningless to another, so run `lore sync --reembed` to wipe the chunk layer and rebuild it with %q",
			stored, want, want), nil)
	}
}

func (s *syncOrchestrator) reembed(ctx context.Context, identity string) error {
	for _, conn := range s.connectors {
		name := conn.Name()
		if err := s.store.SetCursor(ctx, name, nil); err != nil {
			return internalerror.NewInternalError(
				fmt.Sprintf("could not rewind the %s sync cursor for a re-embed", name), err)
		}
	}

	if err := s.store.WipeChunks(ctx); err != nil {
		return internalerror.NewInternalError("could not wipe the chunk layer for a re-embed", err)
	}

	if err := s.store.SetMeta(ctx, metaKeyEmbedderIdentity, identity); err != nil {
		return internalerror.NewInternalError(
			fmt.Sprintf("could not record the embedder identity %q", identity), err)
	}

	return nil
}

// The lease is heartbeated once per batch, so a batch outliving the 60s TTL lets
// another process take it over; this round learns to stop from its next heartbeat.
func (s *syncOrchestrator) syncConnector(ctx context.Context, conn entities.Connector) error {
	name := conn.Name()

	cursor, err := s.store.Cursor(ctx, name)
	if err != nil {
		return internalerror.NewInternalError(
			fmt.Sprintf("could not read the %s sync cursor", name), err)
	}

	for batch, err := range conn.Changes(ctx, cursor) {
		if err != nil {
			return internalerror.NewInternalError(
				fmt.Sprintf("connector %s could not read changes", name), err)
		}

		if err := s.commitBatch(ctx, name, batch); err != nil {
			return err
		}

		if err := s.store.SetCursor(ctx, name, batch.Cursor); err != nil {
			return internalerror.NewInternalError(
				fmt.Sprintf("could not checkpoint the %s sync cursor", name), err)
		}

		if err := s.store.HeartbeatLease(ctx, s.holder); err != nil {
			if errors.Is(err, repositories.ErrLeaseLost) {
				return internalerror.NewPreconditionError(
					"the workspace sync lease is no longer held by this round — another process took it over; retry later",
					err)
			}

			return internalerror.NewInternalError(
				"could not heartbeat the workspace sync lease", err)
		}
	}

	return nil
}

// ReplaceChunks requires the parent document to exist, so documents commit first.
func (s *syncOrchestrator) commitBatch(ctx context.Context, name string, batch entities.Batch) error {
	if err := s.store.UpsertDocuments(ctx, batch.Docs); err != nil {
		return internalerror.NewInternalError(
			fmt.Sprintf("could not store %d documents from %s", len(batch.Docs), name), err)
	}

	for _, doc := range batch.Docs {
		if err := s.indexDocument(ctx, doc); err != nil {
			return err
		}
	}

	return s.links.Link(ctx, batch.Docs)
}

// A document that chunks to nothing still calls ReplaceChunks, or the previous edit's chunks stay retrievable.
func (s *syncOrchestrator) indexDocument(ctx context.Context, doc entities.Document) error {
	chunks := s.chunker.Chunk(doc)

	if len(chunks) > 0 {
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Text
		}

		vectors, err := s.emb.Embed(ctx, texts)
		if err != nil {
			return internalerror.NewInternalError(
				fmt.Sprintf("could not embed the %d chunks of document %q", len(chunks), doc.ID), err)
		}
		if len(vectors) != len(chunks) {
			return internalerror.NewInternalError(fmt.Sprintf(
				"embedder returned %d vectors for the %d chunks of document %q",
				len(vectors), len(chunks), doc.ID), nil)
		}

		for i := range chunks {
			chunks[i].Embedding = vectors[i]
		}
	}

	if err := s.store.ReplaceChunks(ctx, doc.ID, chunks); err != nil {
		return internalerror.NewInternalError(
			fmt.Sprintf("could not store the chunks of document %q", doc.ID), err)
	}

	return nil
}
