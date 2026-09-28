package lore_test

import (
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func TestCapabilitiesString(t *testing.T) {
	tests := []struct {
		name string
		caps lore.Capabilities
		want string
	}{
		{
			name: "nothing declared",
			caps: lore.Capabilities{},
			want: "nothing",
		},
		{
			name: "repo remotes is not a model capability",
			caps: lore.Capabilities{RepoRemotes: true},
			want: "nothing",
		},
		{
			name: "embed only",
			caps: lore.Capabilities{Embed: true},
			want: "embed",
		},
		{
			name: "complete only",
			caps: lore.Capabilities{Complete: true},
			want: "complete",
		},
		{
			name: "both, in the order errors report them",
			caps: lore.Capabilities{Embed: true, Complete: true, RepoRemotes: true},
			want: "embed, complete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.caps.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSplitRemote(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		forge  string
		path   string
		ok     bool
	}{
		{
			name:   "well-formed remote",
			remote: "github:owner/name",
			forge:  "github",
			path:   "owner/name",
			ok:     true,
		},
		{
			name:   "nested path",
			remote: "gitlab:group/sub/project",
			forge:  "gitlab",
			path:   "group/sub/project",
			ok:     true,
		},
		{
			name:   "case is carried through, never folded",
			remote: "github:Owner/Name",
			forge:  "github",
			path:   "Owner/Name",
			ok:     true,
		},
		{
			name:   "only the first colon splits",
			remote: "github:a/b:c",
			forge:  "github",
			path:   "a/b:c",
			ok:     true,
		},
		{
			name:   "no colon",
			remote: "owner/name",
		},
		{
			name:   "empty forge",
			remote: ":owner/name",
		},
		{
			name:   "one-segment path",
			remote: "github:owner",
		},
		{
			name:   "empty middle segment",
			remote: "github:owner//name",
		},
		{
			name:   "trailing slash",
			remote: "github:owner/name/",
		},
		{
			name:   "empty remote",
			remote: "",
		},
		{
			name:   "empty path",
			remote: "github:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forge, path, ok := lore.SplitRemote(tt.remote)
			if ok != tt.ok {
				t.Fatalf("SplitRemote(%q) ok = %v, want %v", tt.remote, ok, tt.ok)
			}
			if !ok {
				return
			}
			if forge != tt.forge || path != tt.path {
				t.Errorf("SplitRemote(%q) = (%q, %q), want (%q, %q)", tt.remote, forge, path, tt.forge, tt.path)
			}
		})
	}
}

func TestIsNamespacedPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "namespace and name", path: "group/project", want: true},
		{name: "nested through a subgroup", path: "group/sub/project", want: true},
		{name: "one segment", path: "project"},
		{name: "empty middle segment", path: "group//project"},
		{name: "trailing slash", path: "group/project/"},
		{name: "empty", path: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lore.IsNamespacedPath(tt.path); got != tt.want {
				t.Errorf("IsNamespacedPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
