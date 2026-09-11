package git

import (
	"context"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func TestPluginNewCodeBindsTheConfiguredRoot(t *testing.T) {
	r := newTestRepo(t)
	r.write("main.go", "package main\n")
	r.commit(authorAda, "2024-05-01T09:30:00Z", "add main")

	repo, err := Plugin().NewCode(lore.CodeConfig{Root: r.root})
	if err != nil {
		t.Fatalf("NewCode: %v", err)
	}

	got, err := repo.HasFileAtHEAD(context.Background(), "main.go")
	if err != nil {
		t.Fatalf("HasFileAtHEAD: %v", err)
	}
	if !got {
		t.Error("the repo does not see the configured clone's HEAD")
	}
}

func TestPluginNewCodeRefusesEmptyRoot(t *testing.T) {
	_, err := Plugin().NewCode(lore.CodeConfig{})
	if err == nil {
		t.Fatal("NewCode accepted an empty root")
	}
	if !strings.Contains(err.Error(), "root is empty") {
		t.Errorf("error = %q, want it to name the empty root", err)
	}
}
