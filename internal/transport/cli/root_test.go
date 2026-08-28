package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"lore/internal/errors/internalerror"
	mock_services "lore/internal/mocks/services"
)

var errUnclassified = errors.New("the disk caught fire")

func fxLikeWrap(err error) error {
	return fmt.Errorf(`could not build arguments for function "lore/internal/di".newIndexStore: %w`, err)
}

type result struct {
	stdout   string
	stderr   string
	exitCode int
	released bool
}

func run(t *testing.T, rt *Runtime, args ...string) result {
	t.Helper()

	var out, errOut bytes.Buffer
	res := result{}

	resolve := func(context.Context, string) (*Runtime, func() error, error) {
		return rt, func() error { res.released = true; return nil }, nil
	}

	root := newRootCommand(resolve)
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)

	err := root.ExecuteContext(context.Background())
	if err != nil {
		res.exitCode = report(&errOut, err)
	}
	res.stdout, res.stderr = out.String(), errOut.String()
	return res
}

func mockQuery(t *testing.T) (*Runtime, *mock_services.MockQueryService) {
	t.Helper()

	query := mock_services.NewMockQueryService(gomock.NewController(t))
	return &Runtime{Query: query}, query
}

func TestReportMapsKindsToExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"nil", nil, exitOK},
		{"bad request", internalerror.NewBadRequestError("--since is not a date", nil), exitBadRequest},
		{"precondition", internalerror.NewPreconditionError("embedder identity mismatch — run `lore sync --reembed`", nil), exitPrecondition},
		{"not found", internalerror.NewNotFoundError("no configuration at ./lore.yaml", nil), exitNotFound},
		{"internal", internalerror.NewInternalError("cannot open the workspace index", nil), exitInternal},
		{"unclassified", errUnclassified, exitInternal},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := report(&stderr, c.err); got != c.code {
				t.Errorf("report = %d, want %d", got, c.code)
			}
			if c.err == nil {
				if stderr.Len() != 0 {
					t.Errorf("stderr = %q, want nothing", stderr.String())
				}
				return
			}
			if !strings.Contains(stderr.String(), c.err.Error()) {
				t.Errorf("stderr = %q, want it to carry %q", stderr.String(), c.err)
			}
		})
	}
}

func TestReportPrintsTheClassifiedMessageOnly(t *testing.T) {
	wrapped := fxLikeWrap(internalerror.NewPreconditionError("another process holds the sync lock", nil))

	var stderr bytes.Buffer
	if got := report(&stderr, wrapped); got != exitPrecondition {
		t.Errorf("report = %d, want %d", got, exitPrecondition)
	}
	if got := stderr.String(); got != "lore: another process holds the sync lock\n" {
		t.Errorf("stderr = %q, want the classified message alone", got)
	}
}

func TestMalformedInvocationsAreBadRequests(t *testing.T) {
	cases := [][]string{
		{"ask"},
		{"ask", "why sqlite?", "and?"},
		{"status", "--nonexistent"},
		{"status", "extra"},
		{"frobnicate"},
	}

	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			res := run(t, &Runtime{}, args...)
			if res.exitCode != exitBadRequest {
				t.Errorf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
			}
			if res.released {
				t.Error("the workspace was built for an invocation that could not run")
			}
		})
	}
}

func TestHelpSucceeds(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"ask", "--help"}, {"init", "--help"}} {
		res := run(t, nil, args...)
		if res.exitCode != exitOK {
			t.Errorf("%v: exit = %d, stderr = %q", args, res.exitCode, res.stderr)
		}
		if res.stdout == "" {
			t.Errorf("%v: printed no help", args)
		}
	}
}
