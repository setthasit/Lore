package plugexec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func embedderOf(t *testing.T, text string, cfg lore.ProviderConfig) (lore.Embedder, error) {
	t.Helper()
	embedder, _, err := embedderWithLogs(t, text, cfg)
	return embedder, err
}

func embedderWithLogs(t *testing.T, text string, cfg lore.ProviderConfig) (lore.Embedder, *syncBuffer, error) {
	t.Helper()
	cfg.Capability = lore.CapabilityEmbed
	plugin, logs := openWithLogs(t, text)
	cfg.Host = testHost(logs)

	built, err := plugin.(lore.ProviderPlugin).NewProvider(cfg)
	if err != nil {
		return nil, logs, err
	}
	embedder, ok := built.(lore.Embedder)
	if !ok {
		t.Fatalf("NewProvider built %T for the embed capability, want a lore.Embedder", built)
	}
	return embedder, logs, nil
}

func completerOf(t *testing.T, text string) lore.Completer {
	t.Helper()
	plugin := mustOpenScript(t, text)

	built, err := plugin.(lore.ProviderPlugin).NewProvider(lore.ProviderConfig{
		Instance:   "scripted-llm",
		Capability: lore.CapabilityComplete,
		Model:      "m",
		Host:       testHost(nil),
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	completer, ok := built.(lore.Completer)
	if !ok {
		t.Fatalf("NewProvider built %T for the complete capability, want a lore.Completer", built)
	}
	return completer
}

func embedLine(vectors, dimensions string) string {
	return `embed emit {"v":1,"id":"$ID","ok":true,"vectors":` + vectors + `,"dimensions":` + dimensions + `}`
}

func TestEachProviderInstanceLogsUnderTheHostItsConfigSupplied(t *testing.T) {
	text := script(providerManifest, "embed stderr embedding\n"+embedLine(`[[0.5,-0.25]]`, "2"), shutdownOK)
	provider := mustOpenScript(t, text).(lore.ProviderPlugin)

	build := func(instance string, host lore.Host) lore.Embedder {
		t.Helper()
		built, err := provider.NewProvider(lore.ProviderConfig{
			Instance:   instance,
			Capability: lore.CapabilityEmbed,
			Model:      "m",
			Dimensions: 2,
			Host:       host,
		})
		if err != nil {
			t.Fatalf("NewProvider(%s): %v", instance, err)
		}
		return built.(lore.Embedder)
	}

	openaiHost, openaiLogs := instanceHost("openai")
	localHost, localLogs := instanceHost("local")
	for _, embedder := range []lore.Embedder{build("openai", openaiHost), build("local", localHost)} {
		if _, err := embedder.Embed(context.Background(), []string{"why B over A"}); err != nil {
			t.Fatalf("Embed: %v", err)
		}
	}

	assertOwnLogsOnly(t,
		instanceLog{instance: "openai", line: "embedding", logs: openaiLogs},
		instanceLog{instance: "local", line: "embedding", logs: localLogs},
	)
}

func TestEmbedReturnsOneVectorPerText(t *testing.T) {
	text := script(providerManifest, embedLine(`[[0.5,-0.25],[0.125,1]]`, "2"), shutdownOK)

	embedder, err := embedderOf(t, text, lore.ProviderConfig{Instance: "vec", Model: "m", Dimensions: 2})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	vectors, err := embedder.Embed(context.Background(), []string{"why B over A", "rollback plan"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	want := [][]float32{{0.5, -0.25}, {0.125, 1}}
	if len(vectors) != len(want) {
		t.Fatalf("got %d vectors, want %d", len(vectors), len(want))
	}
	for i := range want {
		for j := range want[i] {
			if vectors[i][j] != want[i][j] {
				t.Errorf("vectors[%d][%d] = %v, want %v", i, j, vectors[i][j], want[i][j])
			}
		}
	}
	if got := embedder.Dimensions(); got != 2 {
		t.Errorf("Dimensions() = %d, want 2", got)
	}
}

func TestMisalignedVectorsAreAProtocolErrorNotAPartialSuccess(t *testing.T) {
	tests := map[string]struct {
		frame string
		want  string
	}{
		"short": {
			frame: embedLine(`[[0.5,-0.25]]`, "2"),
			want:  "answered 2 texts with 1 vectors",
		},
		"filtered to nothing": {
			frame: embedLine(`[]`, "2"),
			want:  "answered 2 texts with 0 vectors",
		},
		"a vector of the wrong width": {
			frame: embedLine(`[[0.5,-0.25],[0.125]]`, "2"),
			want:  "vectors[1] holds 1 values",
		},
		"a width this instance's index cannot store": {
			frame: embedLine(`[[0.5,-0.25],[0.1,0.2]]`, "768"),
			want:  "vector space is 2 wide",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			embedder, err := embedderOf(t, script(providerManifest, tt.frame, shutdownOK),
				lore.ProviderConfig{Instance: "vec", Model: "m", Dimensions: 2})
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}

			vectors, err := embedder.Embed(context.Background(), []string{"a", "b"})
			if err == nil {
				t.Fatalf("Embed accepted %+v", vectors)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestAWidthNobodyDeclaredIsRefusedWithoutEmbedding(t *testing.T) {
	const reached = "the embed op reached the plugin"
	text := script(providerManifest,
		"embed stderr "+reached+"\n"+embedLine(`[[1,2,3,4]]`, "4"),
		shutdownOK)

	_, logs, err := embedderWithLogs(t, text, lore.ProviderConfig{Instance: "vec", Model: "m"})
	if err == nil {
		t.Fatal("built an embedder for a provider whose width nobody declared")
	}
	if !strings.Contains(err.Error(), "embedder.dimensions must be set") {
		t.Errorf("error %q does not name the width that is missing", err)
	}
	var pluginErr *pluginError
	if !errors.As(err, &pluginErr) || pluginErr.op != opEmbed {
		t.Fatalf("error = %v, want a *pluginError naming %s", err, opEmbed)
	}
	if strings.Contains(logs.String(), reached) {
		t.Error("construction sent an embed request instead of trusting the configured width")
	}
}

func TestEmbeddingNoTextsCostsNoProcess(t *testing.T) {
	// The script has no embed group at all, so a spawned process would die with
	// a non-zero exit and this would be a crash rather than a no-op.
	text := script(providerManifest, shutdownOK)

	embedder, err := embedderOf(t, text, lore.ProviderConfig{Instance: "vec", Model: "m", Dimensions: 2})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	vectors, err := embedder.Embed(context.Background(), nil)
	if err != nil || vectors != nil {
		t.Errorf("Embed(nil) = %v, %v, want no vectors and no error", vectors, err)
	}
}

func TestCompleteReturnsTheAnswer(t *testing.T) {
	text := script(providerManifest,
		`complete emit {"v":1,"id":"$ID","ok":true,"text":"B was chosen because Redis survives a restart."}`,
		shutdownOK)

	got, err := completerOf(t, text).Complete(context.Background(), "answer from evidence", "why B over A?")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.HasPrefix(got, "B was chosen") {
		t.Errorf("Complete = %q, want the plugin's text", got)
	}
}

func TestAnEmptyCompletionIsAnError(t *testing.T) {
	// An empty completion is indistinguishable from a dropped request, so it is
	// reported as internal rather than answered with nothing.
	for name, frame := range map[string]string{
		"empty":           `complete emit {"v":1,"id":"$ID","ok":true,"text":""}`,
		"whitespace-only": `complete emit {"v":1,"id":"$ID","ok":true,"text":"  \t\n "}`,
		"absent":          `complete emit {"v":1,"id":"$ID","ok":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := completerOf(t, script(providerManifest, frame, shutdownOK)).
				Complete(context.Background(), "s", "u")
			if err == nil {
				t.Fatalf("Complete returned %q with no error", got)
			}
			var pluginErr *pluginError
			if !errors.As(err, &pluginErr) || pluginErr.kind != kindInternal {
				t.Fatalf("error = %v, want an internal *pluginError", err)
			}
			if pluginErr.op != opComplete {
				t.Errorf("error names op %q, want %q", pluginErr.op, opComplete)
			}
		})
	}
}

func TestACapabilityTheManifestWithheldIsRefused(t *testing.T) {
	embedOnly := `manifest emit {"v":1,"id":"$ID","ok":true,"manifest":{"name":"scripted","kind":"provider","api_version":1,` +
		`"summary":"s","capabilities":{"embed":true,"complete":false,"repo_remotes":false},"fields":[],"secrets":[]}}`

	plugin := mustOpenScript(t, script(embedOnly, shutdownOK))
	_, err := plugin.(lore.ProviderPlugin).NewProvider(lore.ProviderConfig{
		Instance:   "vec",
		Capability: lore.CapabilityComplete,
		Model:      "m",
		Host:       testHost(nil),
	})
	if err == nil {
		t.Fatal("built a completer from a plugin that declares only embed")
	}
	if !strings.Contains(err.Error(), "does not declare complete") {
		t.Errorf("error %q does not name the missing capability", err)
	}
}
