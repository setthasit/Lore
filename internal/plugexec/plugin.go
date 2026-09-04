package plugexec

import (
	"context"
	"path/filepath"
	"time"

	"github.com/setthasit/Lore/sdk"
)

// Open executes binary once for the manifest handshake and returns the SDK
// plugin interface the manifest's kind names — and only that one, so the
// registry's kind check stays a real check: a binary claiming to be a source
// cannot be bound to an embedder role by an accident of interface satisfaction.
func Open(binary string, host lore.Host) (lore.Plugin, error) {
	return open(binary, host, defaultTuning())
}

func open(binary string, host lore.Host, tune tuning) (lore.Plugin, error) {
	if binary == "" {
		return nil, protocolError("", opManifest, "no plugin binary to execute")
	}

	// The handshake has no instance yet, so the binary's own name is what errors
	// and stderr lines are attributed to until configuration names an instance.
	label := filepath.Base(binary)
	ctx := context.Background()
	session, manifest, err := handshake(ctx, binary, label, host, tune)
	if err != nil {
		return nil, err
	}
	if err := session.close(ctx); err != nil {
		return nil, err
	}

	ext := external{binary: binary, host: host, manifest: manifest, tuning: tune}
	switch manifest.Kind {
	case lore.KindSource:
		// repo_remotes requires a connector that answers MatchesRemote, and the
		// protocol has no op to ask a subprocess that question. Refusing here
		// names the gap; accepting would build a connector whose silent "no"
		// turns the unmatched-clone warning off for every registered clone.
		if manifest.Capabilities.RepoRemotes {
			return nil, protocolError(label, opManifest,
				"plugin %q declares repo_remotes, which an external plugin cannot serve: the protocol has no remote-matching op",
				manifest.Name)
		}
		return &sourcePlugin{external: ext}, nil
	case lore.KindProvider:
		return &providerPlugin{external: ext}, nil
	default:
		return &codePlugin{external: ext}, nil
	}
}

// external is what every instance of one binary shares: where the binary is,
// what the host lends it, and the manifest the handshake agreed on.
type external struct {
	binary   string
	host     lore.Host
	manifest lore.Manifest
	tuning   tuning
}

func (e external) Manifest() lore.Manifest { return e.manifest }

// dial starts the process for one operation and completes its handshake. A
// process lives for one round — spawn, manifest, the operation, shutdown — so
// nothing survives it except what the plugin put in the cursor.
func (e external) dial(ctx context.Context, instance string) (*session, error) {
	session, manifest, err := handshake(ctx, e.binary, instance, e.host, e.tuning)
	if err != nil {
		return nil, err
	}
	// A binary whose manifest changed between processes would run one round's
	// operation under another round's contract; the round it is discovered in is
	// the last one that can still refuse.
	if manifest.Name != e.manifest.Name || manifest.Kind != e.manifest.Kind {
		session.abort()
		return nil, protocolError(instance, opManifest,
			"answered the handshake as %q (%s) after registering as %q (%s)",
			manifest.Name, manifest.Kind, e.manifest.Name, e.manifest.Kind)
	}
	return session, nil
}

// unary runs one request/response op in its own process, which is the whole of
// a provider's or a code plugin's round: spawn, manifest, the op, shutdown.
func (e external) unary(ctx context.Context, instance, op string, timeout time.Duration, build func(envelope) any) (*frame, error) {
	session, err := e.dial(ctx, instance)
	if err != nil {
		return nil, err
	}

	env := session.begin(op)
	if err := session.send(ctx, env, build(env), timeout); err != nil {
		return nil, err
	}
	frame, err := session.await(ctx, env, timeout)
	if err != nil {
		return nil, err
	}
	if !frame.OK {
		session.abort()
		return nil, protocolError(instance, op, "answered %s with neither ok nor an error", op)
	}
	if err := session.close(ctx); err != nil {
		return nil, err
	}
	return frame, nil
}

type sourcePlugin struct {
	external
}

func (p *sourcePlugin) NewSource(cfg lore.SourceConfig) (lore.Connector, error) {
	if cfg.Instance == "" {
		return nil, protocolError(p.manifest.Name, opChanges, "a source instance needs an id: it is the cursor key and the document namespace")
	}
	return &connector{
		external: p.external,
		instance: cfg.Instance,
		config:   emptyObject(cfg.Config),
		secrets:  secretsOrEmpty(cfg.Secrets),
	}, nil
}

type providerPlugin struct {
	external
}

func (p *providerPlugin) NewProvider(cfg lore.ProviderConfig) (lore.Provider, error) {
	if cfg.Instance == "" {
		return nil, protocolError(p.manifest.Name, opEmbed, "a provider instance needs an id")
	}
	if !p.manifest.Capabilities.Declares(cfg.Capability) {
		return nil, protocolError(cfg.Instance, opManifest,
			"plugin %q does not declare %s", p.manifest.Name, cfg.Capability)
	}

	call := call{
		external: p.external,
		instance: cfg.Instance,
		config:   emptyObject(cfg.Config),
		secrets:  secretsOrEmpty(cfg.Secrets),
		model:    cfg.Model,
	}
	// Exactly the asked-for half is built, so the registry's capability
	// assertion still fails a manifest that claims what its binary cannot do.
	switch cfg.Capability {
	case lore.CapabilityEmbed:
		return newEmbedder(call, cfg.Dimensions)
	case lore.CapabilityComplete:
		return &completer{call: call}, nil
	default:
		return nil, protocolError(cfg.Instance, opManifest, "unknown capability %q", cfg.Capability)
	}
}

type codePlugin struct {
	external
}

func (p *codePlugin) NewCode(cfg lore.CodeConfig) (lore.CodeRepo, error) {
	if cfg.Root == "" {
		return nil, protocolError(p.manifest.Name, opBlame, "a code instance needs a clone root")
	}
	return &codeRepo{external: p.external, root: cfg.Root}, nil
}

// Compile-time proof that each kind produces exactly the interface its manifest
// promises, since the registry asserts the same thing at registration.
var (
	_ lore.SourcePlugin   = (*sourcePlugin)(nil)
	_ lore.ProviderPlugin = (*providerPlugin)(nil)
	_ lore.CodePlugin     = (*codePlugin)(nil)
)
