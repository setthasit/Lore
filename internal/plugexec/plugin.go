package plugexec

import (
	"context"
	"path/filepath"
	"time"

	"github.com/setthasit/Lore/sdk"
)

// Open executes binary once for the manifest handshake; the plugin it returns
// implements only the SDK interface its manifest's kind names.
func Open(binary string, host lore.Host) (lore.Plugin, error) {
	return open(binary, host, defaultTuning())
}

func open(binary string, host lore.Host, tune tuning) (lore.Plugin, error) {
	if binary == "" {
		return nil, protocolError("", opManifest, nil, "no plugin binary to execute")
	}

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
		return &sourcePlugin{external: ext}, nil
	case lore.KindProvider:
		return &providerPlugin{external: ext}, nil
	default:
		return &codePlugin{external: ext}, nil
	}
}

type external struct {
	binary   string
	host     lore.Host
	manifest lore.Manifest
	tuning   tuning
}

func (e external) Manifest() lore.Manifest { return e.manifest }

func (e external) withHost(host lore.Host) external {
	e.host = host
	return e
}

func (e external) dial(ctx context.Context, instance string) (*session, error) {
	session, manifest, err := handshake(ctx, e.binary, instance, e.host, e.tuning)
	if err != nil {
		return nil, err
	}
	if manifest.Name != e.manifest.Name || manifest.Kind != e.manifest.Kind {
		session.abort()
		return nil, protocolError(instance, opManifest, nil,
			"answered the handshake as %q (%s) after registering as %q (%s)",
			manifest.Name, manifest.Kind, e.manifest.Name, e.manifest.Kind)
	}
	return session, nil
}

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
		return nil, protocolError(instance, op, nil, "answered %s with neither ok nor an error", op)
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
	return &connector{
		external: p.withHost(cfg.Host),
		instance: cfg.Instance,
		config:   emptyObject(cfg.Config),
		secrets:  secretsOrEmpty(cfg.Secrets),
	}, nil
}

type providerPlugin struct {
	external
}

func (p *providerPlugin) NewProvider(cfg lore.ProviderConfig) (lore.Provider, error) {
	if !p.manifest.Capabilities.Declares(cfg.Capability) {
		return nil, protocolError(cfg.Instance, opManifest, nil,
			"plugin %q does not declare %s", p.manifest.Name, cfg.Capability)
	}

	call := call{
		external: p.withHost(cfg.Host),
		instance: cfg.Instance,
		config:   emptyObject(cfg.Config),
		secrets:  secretsOrEmpty(cfg.Secrets),
		model:    cfg.Model,
	}
	switch cfg.Capability {
	case lore.CapabilityEmbed:
		return newEmbedder(call, cfg.Dimensions)
	case lore.CapabilityComplete:
		return &completer{call: call}, nil
	default:
		return nil, protocolError(cfg.Instance, opManifest, nil, "unknown capability %q", cfg.Capability)
	}
}

type codePlugin struct {
	external
}

func (p *codePlugin) NewCode(cfg lore.CodeConfig) (lore.CodeRepo, error) {
	if cfg.Root == "" {
		return nil, protocolError(p.manifest.Name, opBlame, nil, "a code instance needs a clone root")
	}
	return &codeRepo{external: p.withHost(cfg.Host), root: cfg.Root}, nil
}

var (
	_ lore.SourcePlugin   = (*sourcePlugin)(nil)
	_ lore.ProviderPlugin = (*providerPlugin)(nil)
	_ lore.CodePlugin     = (*codePlugin)(nil)
)
