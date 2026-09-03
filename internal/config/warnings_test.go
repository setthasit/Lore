package config

import (
	"strings"
	"testing"
)

func githubRepos(repos ...string) Sources {
	return Sources{GitHub: &GitHubSource{TokenEnv: "LORE_GITHUB_TOKEN", Repos: repos}}
}

func gitlabProjects(projects ...string) Sources {
	return Sources{GitLab: &GitLabSource{TokenEnv: "LORE_GITLAB_TOKEN", Projects: projects}}
}

func TestStartupWarnings(t *testing.T) {
	const clone = "/home/dev/myproject"

	tests := []struct {
		name        string
		sources     Sources
		repos       []Repo
		wantRemotes []string
	}{
		{
			name:    "a remote naming an ingested repo is silent",
			sources: githubRepos("acme/myproject"),
			repos:   []Repo{{Path: clone, Remote: "github:acme/myproject"}},
		},
		{
			name:    "a remote differing only in case still matches",
			sources: githubRepos("ACME/MyProject"),
			repos:   []Repo{{Path: clone, Remote: "github:acme/myproject"}},
		},
		{
			name:    "a clone without a remote is a blame-only registration",
			sources: githubRepos("acme/myproject"),
			repos:   []Repo{{Path: clone}},
		},
		{
			name:        "a remote naming an uningested repo warns",
			sources:     githubRepos("acme/yes"),
			repos:       []Repo{{Path: clone, Remote: "github:acme/nope"}},
			wantRemotes: []string{"github:acme/nope"},
		},
		{
			name:        "a remote without a source prefix warns",
			sources:     githubRepos("acme/myproject"),
			repos:       []Repo{{Path: clone, Remote: "acme/myproject"}},
			wantRemotes: []string{"acme/myproject"},
		},
		{
			name:    "a gitlab remote naming an ingested project is silent",
			sources: gitlabProjects("acme/myproject"),
			repos:   []Repo{{Path: clone, Remote: "gitlab:acme/myproject"}},
		},
		{
			name:    "a gitlab remote may nest through subgroups",
			sources: gitlabProjects("acme/platform/infra"),
			repos:   []Repo{{Path: clone, Remote: "gitlab:acme/platform/infra"}},
		},
		{
			name:        "a gitlab remote naming an uningested project warns",
			sources:     gitlabProjects("acme/yes"),
			repos:       []Repo{{Path: clone, Remote: "gitlab:acme/nope"}},
			wantRemotes: []string{"gitlab:acme/nope"},
		},
		{
			name:        "a gitlab remote without a gitlab source warns",
			sources:     githubRepos("acme/myproject"),
			repos:       []Repo{{Path: clone, Remote: "gitlab:acme/myproject"}},
			wantRemotes: []string{"gitlab:acme/myproject"},
		},
		{
			name:        "a remote with an empty owner warns",
			sources:     githubRepos("acme/myproject"),
			repos:       []Repo{{Path: clone, Remote: "github:/myproject"}},
			wantRemotes: []string{"github:/myproject"},
		},
		{
			name:        "a remote with an empty name warns",
			sources:     githubRepos("acme/myproject"),
			repos:       []Repo{{Path: clone, Remote: "github:acme/"}},
			wantRemotes: []string{"github:acme/"},
		},
		{
			name:        "a remote carrying more than owner and name warns",
			sources:     githubRepos("acme/myproject"),
			repos:       []Repo{{Path: clone, Remote: "github:acme/myproject/sub"}},
			wantRemotes: []string{"github:acme/myproject/sub"},
		},
		{
			name:        "a github remote without a github source warns",
			sources:     Sources{Notion: &NotionSource{TokenEnv: "LORE_NOTION_TOKEN"}},
			repos:       []Repo{{Path: clone, Remote: "github:acme/myproject"}},
			wantRemotes: []string{"github:acme/myproject"},
		},
		{
			name:    "every unmapped clone warns",
			sources: githubRepos("acme/yes"),
			repos: []Repo{
				{Path: clone, Remote: "github:acme/yes"},
				{Path: "/home/dev/one", Remote: "github:acme/one"},
				{Path: "/home/dev/two"},
				{Path: "/home/dev/three", Remote: "jira:acme/three"},
			},
			wantRemotes: []string{"github:acme/one", "jira:acme/three"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Workspace: "myproject", Sources: test.sources, Repos: test.repos}

			got := cfg.StartupWarnings()

			if len(test.wantRemotes) == 0 {
				if got != nil {
					t.Fatalf("StartupWarnings() = %q, want nil", got)
				}
				return
			}
			if len(got) != len(test.wantRemotes) {
				t.Fatalf("StartupWarnings() = %q, want %d warning(s)", got, len(test.wantRemotes))
			}
			for i, remote := range test.wantRemotes {
				if !strings.Contains(got[i], remote) {
					t.Errorf("StartupWarnings()[%d] = %q, want it to name remote %q", i, got[i], remote)
				}
			}
		})
	}
}

func TestStartupWarningNamesTheCloneAndTheConsequence(t *testing.T) {
	cfg := &Config{
		Workspace: "myproject",
		Sources:   githubRepos("acme/yes"),
		Repos:     []Repo{{Path: "/home/dev/nope", Remote: "github:acme/nope"}},
	}

	got := cfg.StartupWarnings()

	if len(got) != 1 {
		t.Fatalf("StartupWarnings() = %q, want one warning", got)
	}
	for _, want := range []string{"/home/dev/nope", "github:acme/nope", "blame still works", "chains stop at the commit layer"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("StartupWarnings()[0] = %q, want it to contain %q", got[0], want)
		}
	}
}
