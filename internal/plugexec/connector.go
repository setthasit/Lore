package plugexec

import (
	"context"
	"encoding/json"
	"iter"

	"github.com/setthasit/Lore/sdk"
)

// connector is one configured source instance. It holds no process: a process
// exists for one stream, so an idle instance costs nothing between rounds.
type connector struct {
	external
	instance string
	config   json.RawMessage
	secrets  map[string]string
}

var _ lore.Connector = (*connector)(nil)

func (c *connector) Name() string { return c.instance }

func (c *connector) Changes(ctx context.Context, cursor lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) {
		session, err := c.dial(ctx, c.instance)
		if err != nil {
			yield(lore.Batch{}, err)
			return
		}

		env := session.begin(opChanges)
		request := changesRequest{
			envelope: env,
			Instance: c.instance,
			Config:   c.config,
			Secrets:  c.secrets,
			Cursor:   cursorOrEmpty(cursor),
		}
		if err := session.send(ctx, env, request, c.tuning.unary); err != nil {
			yield(lore.Batch{}, err)
			return
		}

		for {
			// The next frame is read only here, after the consumer has taken the
			// previous batch: that makes the OS pipe the backpressure, so a plugin
			// faster than the indexer blocks on write instead of the host buffering
			// a whole source in memory. Reading ahead into a channel would trade a
			// bounded pipe for an unbounded heap.
			frame, err := session.await(ctx, env, c.tuning.idle)
			if err != nil {
				yield(lore.Batch{}, err)
				return
			}

			switch {
			case frame.Done:
				// The stream ended; the round's last act is the ordered shutdown,
				// and a plugin that fails to leave is reported even though every
				// batch before it is already committed.
				if err := session.close(ctx); err != nil {
					yield(lore.Batch{}, err)
				}
				return
			case frame.Batch == nil:
				session.abort()
				yield(lore.Batch{}, protocolError(c.instance, opChanges,
					"answered changes with a frame carrying neither a batch nor done"))
				return
			case frame.Batch.Cursor == nil || len(*frame.Batch.Cursor) == 0:
				// The batch is the checkpoint unit: the host commits the documents
				// and then persists that frame's cursor. A frame without one makes
				// crash-safe resume unimplementable, so it is refused rather than
				// committed against the cursor of an earlier batch.
				session.abort()
				yield(lore.Batch{}, protocolError(c.instance, opChanges,
					"sent a batch of %d documents without a cursor, so committing it would checkpoint nothing",
					len(frame.Batch.Docs)))
				return
			}

			if !yield(lore.Batch{Docs: frame.Batch.Docs, Cursor: *frame.Batch.Cursor}, nil) {
				// The consumer abandoned the stream, which is a cancellation: the
				// plugin is escalated away rather than left streaming into a pipe
				// nobody reads.
				session.abort()
				return
			}
		}
	}
}

// MatchesRemote answers whether a registered clone's remote is one this
// instance ingests. It exists so a source's repo_remotes capability works the
// same out of process as in: the alternative was refusing the capability to
// external plugins, which would have made the unmatched-clone warning a
// privilege of compiled code.
//
// The question is asked over its own op rather than derived from the config,
// because only the plugin knows how its own repository identifiers compare —
// GitHub's are case-insensitive, GitLab's are not.
func (c *connector) MatchesRemote(remote string) bool {
	if remote == "" {
		return false
	}

	// A startup warning must never be the reason a workspace fails to start, so
	// an unreachable or unhelpful plugin reads as "not mine" and the operator
	// gets the warning rather than an error.
	frame, err := c.unary(context.Background(), c.instance, opRemote, c.tuning.unary, func(env envelope) any {
		return remoteRequest{
			envelope: env,
			Instance: c.instance,
			Config:   c.config,
			Secrets:  c.secrets,
			Remote:   remote,
		}
	})
	if err != nil {
		if c.host.Log != nil {
			c.host.Log.Debug("plugin could not answer whether it ingests a clone's remote",
				"instance", c.instance, "remote", remote, "error", err)
		}
		return false
	}
	return frame.Matches
}
