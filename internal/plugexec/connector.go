package plugexec

import (
	"context"
	"encoding/json"
	"iter"

	"github.com/setthasit/Lore/sdk"
)

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
			frame, err := session.await(ctx, env, c.tuning.idle)
			if err != nil {
				yield(lore.Batch{}, err)
				return
			}

			switch {
			case frame.Done:
				if err := session.close(ctx); err != nil {
					yield(lore.Batch{}, err)
				}
				return
			case frame.Batch == nil:
				session.abort()
				yield(lore.Batch{}, protocolError(c.instance, opChanges, nil,
					"answered changes with a frame carrying neither a batch nor done"))
				return
			case frame.Batch.Cursor == nil || len(*frame.Batch.Cursor) == 0:
				session.abort()
				yield(lore.Batch{}, protocolError(c.instance, opChanges, nil,
					"sent a batch of %d documents without a cursor, so committing it would checkpoint nothing",
					len(frame.Batch.Docs)))
				return
			}

			if !yield(lore.Batch{Docs: frame.Batch.Docs, Cursor: *frame.Batch.Cursor}, nil) {
				session.abort()
				return
			}
		}
	}
}

// GitHub's repository identifiers are case-insensitive and GitLab's are not, so only the plugin can compare them.
func (c *connector) MatchesRemote(remote string) bool {
	if remote == "" || !c.manifest.Capabilities.RepoRemotes {
		return false
	}

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
		c.host.Log.Debug("plugin could not answer whether it ingests a clone's remote",
			"remote", remote, "error", err)
		return false
	}
	return frame.Matches
}
