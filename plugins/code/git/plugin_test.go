package git

import (
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func TestPluginNewCode(t *testing.T) {
	repo, err := Plugin().NewCode(lore.CodeConfig{Root: t.TempDir(), Remote: "github:acme/app"})
	if err != nil {
		t.Fatalf("NewCode: %v", err)
	}
	if repo == nil {
		t.Fatal("NewCode returned no repo")
	}
}

// A repo bound to nothing would run every git command against the process's
// working directory and answer with another clone's history.
func TestPluginNewCodeRefusesEmptyRoot(t *testing.T) {
	_, err := Plugin().NewCode(lore.CodeConfig{})
	if err == nil {
		t.Fatal("NewCode accepted an empty root")
	}

	const want = "git: root is empty"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}
