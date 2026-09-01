package services_test

import (
	"context"
	"strings"
	"testing"

	"lore/internal/errors/internalerror"
	"lore/internal/services"
)

const (
	askOnlyMessage = "no repositories registered — code anchoring disabled for this workspace"
	blameRemedy    = "ask find_decision instead"
	whyFile        = "internal/auth/auth.go"
	whyRemote      = "github:acme/lore"
	whyPath        = "/home/dev/lore"
	otherPath      = "/home/dev/tools"
)

var (
	oneRepo  = []services.CodeRepo{{Path: whyPath, Remote: whyRemote}}
	twoRepos = []services.CodeRepo{
		{Path: whyPath, Remote: whyRemote},
		{Path: otherPath},
	}
)

func whySpan(start, end int) services.WhyRequest {
	return services.WhyRequest{File: whyFile, LineStart: start, LineEnd: end}
}

func TestWhyValidationOrder(t *testing.T) {
	tests := []struct {
		name     string
		repos    []services.CodeRepo
		req      services.WhyRequest
		kind     internalerror.Kind
		message  string
		contains []string
	}{
		{
			name:    "no repositories registered refuses code anchoring",
			req:     whySpan(10, 20),
			kind:    internalerror.KindPrecondition,
			message: askOnlyMessage,
		},
		{
			name:    "no repositories outranks every other complaint",
			req:     services.WhyRequest{LineStart: -3, LineEnd: -9},
			kind:    internalerror.KindPrecondition,
			message: askOnlyMessage,
		},
		{
			name:     "a blank file is rejected",
			repos:    oneRepo,
			req:      services.WhyRequest{File: "   ", LineStart: 10, LineEnd: 20},
			kind:     internalerror.KindBadRequest,
			contains: []string{"file"},
		},
		{
			name:     "a zero line_start is rejected",
			repos:    oneRepo,
			req:      whySpan(0, 20),
			kind:     internalerror.KindBadRequest,
			contains: []string{"line_start 0"},
		},
		{
			name:     "a negative line_start is rejected",
			repos:    oneRepo,
			req:      whySpan(-3, 20),
			kind:     internalerror.KindBadRequest,
			contains: []string{"line_start -3"},
		},
		{
			name:     "a line_end before line_start is rejected",
			repos:    oneRepo,
			req:      whySpan(40, 12),
			kind:     internalerror.KindBadRequest,
			contains: []string{"40-12"},
		},
		{
			name:     "a zero line_end is the single line line_start",
			repos:    oneRepo,
			req:      whySpan(12, 0),
			kind:     internalerror.KindPrecondition,
			contains: []string{blameRemedy},
		},
		{
			name:     "an unregistered repo names the request and the registered repos",
			repos:    twoRepos,
			req:      services.WhyRequest{Repo: "github:acme/other", File: whyFile, LineStart: 10, LineEnd: 20},
			kind:     internalerror.KindNotFound,
			contains: []string{"github:acme/other", whyRemote, otherPath},
		},
		{
			name:     "an unregistered repo outranks an invalid span",
			repos:    twoRepos,
			req:      services.WhyRequest{Repo: "github:acme/other", File: whyFile, LineStart: 40, LineEnd: 12},
			kind:     internalerror.KindNotFound,
			contains: []string{"github:acme/other"},
		},
		{
			name:     "an omitted repo selects the only registered one",
			repos:    oneRepo,
			req:      whySpan(10, 20),
			kind:     internalerror.KindPrecondition,
			contains: []string{blameRemedy},
		},
		{
			name:     "an omitted repo with several registered asks for one",
			repos:    twoRepos,
			req:      whySpan(10, 20),
			kind:     internalerror.KindBadRequest,
			contains: []string{"repo must name one", whyRemote, otherPath},
		},
		{
			name:     "a repo named by its remote is registered",
			repos:    twoRepos,
			req:      services.WhyRequest{Repo: whyRemote, File: whyFile, LineStart: 10, LineEnd: 20},
			kind:     internalerror.KindPrecondition,
			contains: []string{blameRemedy},
		},
		{
			name:     "a repo named by its path is registered",
			repos:    twoRepos,
			req:      services.WhyRequest{Repo: otherPath, File: whyFile, LineStart: 10, LineEnd: 20},
			kind:     internalerror.KindPrecondition,
			contains: []string{blameRemedy},
		},
		{
			name:     "a well-formed request reports the missing blame capability",
			repos:    oneRepo,
			req:      services.WhyRequest{Repo: whyPath, File: whyFile, LineStart: 10, LineEnd: 20, Question: "why is this locked?"},
			kind:     internalerror.KindPrecondition,
			contains: []string{"code anchoring", blameRemedy},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle, err := services.NewWhyService(tt.repos).Why(context.Background(), tt.req)

			if bundle != nil {
				t.Errorf("bundle = %+v, want none alongside a refusal", bundle)
			}
			if got := internalerror.KindOf(err); got != tt.kind {
				t.Fatalf("kind = %s, want %s (error %v)", got, tt.kind, err)
			}
			if tt.message != "" && err.Error() != tt.message {
				t.Errorf("message = %q, want %q", err.Error(), tt.message)
			}
			for _, want := range tt.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message = %q, want it to name %q", err.Error(), want)
				}
			}
		})
	}
}

func TestWhyKeepsTheReposItWasConstructedWith(t *testing.T) {
	repos := []services.CodeRepo{{Path: whyPath, Remote: whyRemote}}
	svc := services.NewWhyService(repos)
	repos[0] = services.CodeRepo{Path: otherPath, Remote: "github:acme/rewritten"}

	_, err := svc.Why(context.Background(), services.WhyRequest{Repo: whyRemote, File: whyFile, LineStart: 10})

	if internalerror.KindOf(err) != internalerror.KindPrecondition {
		t.Fatalf("error = %v, want the repo registered at construction to still match", err)
	}
}
