package plugexec

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func codeOf(t *testing.T, text, root string) lore.CodeRepo {
	t.Helper()
	plugin := mustOpenScript(t, text)

	code, ok := plugin.(lore.CodePlugin)
	if !ok {
		t.Fatalf("a code manifest produced %T, want a lore.CodePlugin", plugin)
	}
	repo, err := code.NewCode(lore.CodeConfig{Root: root, Remote: "github:acme/app"})
	if err != nil {
		t.Fatalf("NewCode: %v", err)
	}
	return repo
}

func TestBlameReturnsSpansForAWorkspaceAbsolutePath(t *testing.T) {
	root := t.TempDir()
	text := script(codeManifest,
		`blame emit {"v":1,"id":"$ID","ok":true,"spans":[{"sha":"9c1f0ab3e5d4","line_start":40,"line_end":42,`+
			`"author":"Ada Lovelace","time":"2026-05-14T08:31:02Z","lines":["if !tok.Valid() {","\treturn errUnauthorized","}"]}],`+
			`"text":"$PATH"}`,
		shutdownOK)

	spans, err := codeOf(t, text, root).Blame(context.Background(), "internal/auth/auth.go", 40, 42)
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].SHA != "9c1f0ab3e5d4" || spans[0].LineStart != 40 || spans[0].LineEnd != 42 {
		t.Errorf("span = %+v, want the one the plugin sent", spans[0])
	}
	if len(spans[0].Lines) != 3 {
		t.Errorf("span holds %d lines, want one per line in the span", len(spans[0].Lines))
	}
	if spans[0].Time.IsZero() {
		t.Error("span time did not decode")
	}
}

func TestTheHostResolvesThePathAgainstTheCloneRoot(t *testing.T) {
	root := t.TempDir()
	// The plugin echoes the path it was sent back in an unused field, which is
	// the only way to observe what crossed the pipe.
	text := script(codeManifest, `log emit {"v":1,"id":"$ID","ok":true,"commits":[{"sha":"$PATH","author":"a","time":"2026-05-14T08:31:02Z","subject":"s"}]}`, shutdownOK)

	commits, err := codeOf(t, text, root).Log(context.Background(), "internal/auth/auth.go")
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	want := filepath.Join(root, "internal", "auth", "auth.go")
	if commits[0].SHA != want {
		t.Errorf("the plugin was sent %q, want the workspace-absolute %q", commits[0].SHA, want)
	}
}

func TestAPathThatClimbsOutOfTheCloneIsRefused(t *testing.T) {
	root := filepath.Join(t.TempDir(), "clone")
	text := script(codeManifest, `log emit {"v":1,"id":"$ID","ok":true,"commits":[]}`, shutdownOK)
	repo := codeOf(t, text, root)

	for _, path := range []string{"../secrets.env", "internal/../../etc/passwd"} {
		if _, err := repo.Log(context.Background(), path); err == nil {
			t.Errorf("Log(%q) was sent to the plugin, want a refusal", path)
		} else if !strings.Contains(err.Error(), "climbs out of the clone") {
			t.Errorf("Log(%q) error = %q, want it to name the escape", path, err)
		}
	}
}

func TestLogWithNoHistoryIsAnAnswerNotAnError(t *testing.T) {
	text := script(codeManifest, `log emit {"v":1,"id":"$ID","ok":true,"commits":[]}`, shutdownOK)

	commits, err := codeOf(t, text, t.TempDir()).Log(context.Background(), "untracked.go")
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(commits) != 0 {
		t.Errorf("got %d commits, want none", len(commits))
	}
}

func TestHasFileAtHEADAnswersFalseWithoutFailing(t *testing.T) {
	// A directory, an untracked path and a clone with no commits are all false
	// and none of them is an error, which is the whole reason the op exists
	// instead of inferring presence from an empty log.
	for name, tt := range map[string]struct {
		frame string
		want  bool
	}{
		"present":   {frame: `has_file emit {"v":1,"id":"$ID","ok":true,"present":true}`, want: true},
		"absent":    {frame: `has_file emit {"v":1,"id":"$ID","ok":true,"present":false}`},
		"unstated":  {frame: `has_file emit {"v":1,"id":"$ID","ok":true}`},
		"directory": {frame: `has_file emit {"v":1,"id":"$ID","ok":true,"present":false}`},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := codeOf(t, script(codeManifest, tt.frame, shutdownOK), t.TempDir()).
				HasFileAtHEAD(context.Background(), "internal/auth/auth.go")
			if err != nil {
				t.Fatalf("HasFileAtHEAD: %v", err)
			}
			if got != tt.want {
				t.Errorf("HasFileAtHEAD = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAFailureToReadTheCloneIsAnError(t *testing.T) {
	text := script(codeManifest,
		`has_file emit {"v":1,"id":"$ID","error":{"message":"not a git repository","retryable":false,"kind":"invalid_config"}}`,
		shutdownOK)

	if _, err := codeOf(t, text, t.TempDir()).HasFileAtHEAD(context.Background(), "auth.go"); err == nil {
		t.Fatal("a clone that cannot be read was reported as a missing file")
	}
}

func TestCodeRequestsCarryNoConfigAndNoSecrets(t *testing.T) {
	// A local clone needs no credentials, so the payload has nowhere to leak one.
	session := &session{instance: "git", idPrefix: "abc"}
	env := session.begin(opBlame)

	line, err := marshalRequest(blameRequest{envelope: env, Path: "/w/api/auth.go", StartLine: 1, EndLine: 2})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"config", "secrets"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("blame request %s carries %q", line, forbidden)
		}
	}
	for _, want := range []string{`"op":"blame"`, `"path":"/w/api/auth.go"`, `"start_line":1`, `"end_line":2`} {
		if !strings.Contains(line, want) {
			t.Errorf("blame request %s does not carry %s", line, want)
		}
	}
}
