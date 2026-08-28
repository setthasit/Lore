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

// metaKeyEmbedderIdentity is where a round records the vector space its chunks
// live in. The store owns the keys describing the file's own identity (schema
// generation, vector width); the embedder's identity is the service layer's,
// because only this layer knows which embedder is configured.
const metaKeyEmbedderIdentity = "embedder_identity"

// SyncOptions carries the flags a caller may set on a sync round.
type SyncOptions struct {
	// Reembed rebuilds the whole chunk layer against the currently configured
	// embedder: chunks are wiped, every connector's cursor is reset, and the
	// round re-reads its sources from the beginning. It is the remedy for an
	// embedder identity mismatch and is what `lore sync --reembed` sets.
	Reembed bool
}

// SyncOrchestrator runs a sync round: it holds the workspace's sync lease,
// streams each configured connector's changes into the index, and checkpoints
// at batch granularity so an interrupted round resumes where it stopped.
//
// A round is the only writer of the derived index. It is safe to run
// concurrently with other processes sharing the workspace file only in the sense
// that the lease makes the losers skip: two rounds never write at once.
type SyncOrchestrator interface {
	// Sync runs one round over every configured connector, oldest changes
	// first, and returns once they are all exhausted.
	//
	// It reports a precondition error when another process holds the sync lease
	// and when the index's embedder identity no longer matches the configured
	// embedder; every other failure is internal. A failed round leaves the
	// index consistent up to the last committed batch: cursors only ever
	// advance past documents that are durably stored, so re-running resumes
	// without gaps and without re-reading what already landed.
	Sync(ctx context.Context, opts SyncOptions) error
}

type syncOrchestrator struct {
	store      repositories.IndexStore
	connectors []entities.Connector
	chunker    Chunker
	emb        embedder.Embedder
	holder     string
}

var _ SyncOrchestrator = (*syncOrchestrator)(nil)

// NewSyncOrchestrator wires a sync round over the configured connectors. An
// empty connectors slice is legitimate: a workspace declares its sources, and a
// round over none of them is a round that does nothing.
func NewSyncOrchestrator(
	store repositories.IndexStore,
	connectors []entities.Connector,
	chunker Chunker,
	emb embedder.Embedder,
) SyncOrchestrator {
	return &syncOrchestrator{
		store:      store,
		connectors: connectors,
		chunker:    chunker,
		emb:        emb,
		holder:     leaseHolder(),
	}
}

// leaseHolder names this process in the workspace's sync lease. Host and PID
// together distinguish the `lore serve` daemon from an ad-hoc CLI run on the
// same machine and from another machine sharing the workspace file, which is
// what makes a held lease diagnosable rather than merely blocking. A host with
// no resolvable name still yields a distinct holder through its PID.
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
	// The lease is released even when ctx is already cancelled: a round that
	// stops early must not leave the next one waiting out the TTL. A release
	// that fails anyway is not worth reporting over the round's own outcome —
	// the TTL bounds the damage to one skipped scheduler tick.
	defer func() { _ = s.store.ReleaseLease(context.WithoutCancel(ctx), s.holder) }()

	if err := s.reconcileIdentity(ctx, opts); err != nil {
		return err
	}

	for _, conn := range s.connectors {
		if err := s.syncConnector(ctx, conn); err != nil {
			return err
		}
	}

	return nil
}

// reconcileIdentity settles which vector space this round writes into before it
// writes anything.
//
// Vectors are only comparable within one embedder's space, so an index whose
// stored identity differs from the configured embedder cannot be extended — a
// round that appended new vectors to old ones would silently degrade recall
// instead of failing. The mismatch is therefore a precondition error naming its
// remedy, never a silent re-embed: rebuilding the chunk layer costs real
// embedding spend and is the caller's decision.
//
// Rebuilding is delegated to reembed, which owns the crash-safe ordering.
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
		// First sync of this workspace: the index has no vectors yet, so the
		// configured embedder defines the space rather than conflicting with it.
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

// reembed rewinds every connector, clears the chunk layer, and only then records
// the new identity, so the round that follows is a full backfill.
//
// The order is what makes a crashed re-embed recoverable. Rewinding first means
// any interruption leaves cursors pointing at the beginning, so the next round
// re-streams every document and rebuilds its chunks through ReplaceChunks —
// stale chunks that survived an unfinished wipe are replaced rather than
// orphaned. Wiping first would strand the reverse case: a crash between the wipe
// and the last rewind leaves un-rewound connectors with no chunks and a cursor
// that skips the documents which would rebuild them, and if the rebuild was for
// an unchanged identity nothing would ever flag it. Recording the identity last
// keeps the mismatch error standing until the rebuild is actually set up.
func (s *syncOrchestrator) reembed(ctx context.Context, identity string) error {
	// Documents survive a wipe, but their chunks do not, so every connector
	// must stream its history again to rebuild them.
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

// syncConnector streams one connector's changes into the index, checkpointing
// after each batch.
//
// The lease is heartbeated once per batch rather than from a timer goroutine:
// the round is a single sequential writer, so the only place it can prove
// liveness is between batches, and a goroutine would only keep a wedged round's
// lease alive. Known limit: a batch that takes longer than the 60s lease TTL
// lets the lease lapse and another process take it over. The heartbeat after
// that batch then fails, which is exactly how this round learns to stop — but
// the two rounds do overlap for the length of that batch. Connectors keep
// batches small (a page of results) so this stays theoretical.
func (s *syncOrchestrator) syncConnector(ctx context.Context, conn entities.Connector) error {
	name := conn.Name()

	cursor, err := s.store.Cursor(ctx, name)
	if err != nil {
		return internalerror.NewInternalError(
			fmt.Sprintf("could not read the %s sync cursor", name), err)
	}

	for batch, err := range conn.Changes(ctx, cursor) {
		// Abandoning the range stops the connector's iterator. The cursor of
		// the last fully committed batch stays durable, so the next round
		// resumes from there instead of from the batch that failed.
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
			// Only a lost lease is the caller's to act on; a store that
			// cannot be reached is an ordinary infrastructure failure and
			// telling the user someone took their lease would be a lie.
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

// commitBatch makes one batch durable: its documents first, then the chunks
// derived from each of them. The order matters — ReplaceChunks requires the
// parent document to exist.
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

	return nil
}

// indexDocument rebuilds one document's chunk set: chunk, embed in a single call
// per document, then replace.
//
// A document that chunks to nothing — an empty body, or a body emptied by an
// edit — still calls ReplaceChunks, with no chunks. Skipping the call would
// leave the previous edit's chunks retrievable forever, citing text the document
// no longer contains.
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
		// The Embedder contract promises one vector per text, positionally
		// aligned. Checking turns a provider that breaks it into a classified
		// error instead of an index-out-of-range panic mid-round.
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
