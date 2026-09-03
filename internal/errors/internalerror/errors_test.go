package internalerror_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

var errCause = errors.New("disk offline")

type constructorCase struct {
	name string
	new  func(message string, cause error) error
	kind internalerror.Kind
}

func constructors() []constructorCase {
	return []constructorCase{
		{"NewBadRequestError", internalerror.NewBadRequestError, internalerror.KindBadRequest},
		{"NewNotFoundError", internalerror.NewNotFoundError, internalerror.KindNotFound},
		{"NewPreconditionError", internalerror.NewPreconditionError, internalerror.KindPrecondition},
		{"NewInternalError", internalerror.NewInternalError, internalerror.KindInternal},
	}
}

func TestErrorMessageFormatting(t *testing.T) {
	t.Parallel()

	formats := []struct {
		name  string
		cause error
		want  string
	}{
		{"nil cause keeps the message verbatim", nil, "no repositories registered"},
		{"cause is appended after a colon", errCause, "no repositories registered: disk offline"},
		{"nested cause chain is rendered in full", fmt.Errorf("open index: %w", errCause), "no repositories registered: open index: disk offline"},
	}

	for _, c := range constructors() {
		for _, f := range formats {
			t.Run(c.name+"/"+f.name, func(t *testing.T) {
				err := c.new("no repositories registered", f.cause)
				if got := err.Error(); got != f.want {
					t.Fatalf("Error() = %q, want %q", got, f.want)
				}
			})
		}
	}
}

func TestUnwrapReturnsCause(t *testing.T) {
	t.Parallel()

	for _, c := range constructors() {
		t.Run(c.name, func(t *testing.T) {
			if got := errors.Unwrap(c.new("classified", errCause)); got != errCause {
				t.Fatalf("Unwrap() = %v, want %v", got, errCause)
			}
			if got := errors.Unwrap(c.new("classified", nil)); got != nil {
				t.Fatalf("Unwrap() = %v, want nil for nil-cause construction", got)
			}
		})
	}
}

func TestErrorsIsFindsWrappedCause(t *testing.T) {
	t.Parallel()

	for _, c := range constructors() {
		t.Run(c.name, func(t *testing.T) {
			chains := []struct {
				name   string
				err    error
				wantIs bool
			}{
				{"direct cause", c.new("classified", errCause), true},
				{"nested cause", c.new("classified", fmt.Errorf("open index: %w", errCause)), true},
				{"re-wrapped by %w", fmt.Errorf("find decision: %w", c.new("classified", errCause)), true},
				{"nil cause", c.new("classified", nil), false},
			}

			for _, chain := range chains {
				if got := errors.Is(chain.err, errCause); got != chain.wantIs {
					t.Errorf("errors.Is(%s, errCause) = %t, want %t", chain.name, got, chain.wantIs)
				}
			}
		})
	}
}

func TestErrorsAsExposesKindAndMessage(t *testing.T) {
	t.Parallel()

	for _, c := range constructors() {
		t.Run(c.name, func(t *testing.T) {
			err := fmt.Errorf("service: %w", c.new("embedder identity mismatch", errCause))

			var classified *internalerror.Error
			if !errors.As(err, &classified) {
				t.Fatalf("errors.As did not find *internalerror.Error in %v", err)
			}
			if classified.Kind != c.kind {
				t.Errorf("Kind = %v, want %v", classified.Kind, c.kind)
			}
			if classified.Message != "embedder identity mismatch" {
				t.Errorf("Message = %q, want %q", classified.Message, "embedder identity mismatch")
			}
		})
	}
}

func TestClassificationMatrix(t *testing.T) {
	t.Parallel()

	predicates := []struct {
		name string
		fn   func(error) bool
		kind internalerror.Kind
	}{
		{"IsBadRequest", internalerror.IsBadRequest, internalerror.KindBadRequest},
		{"IsNotFound", internalerror.IsNotFound, internalerror.KindNotFound},
		{"IsPrecondition", internalerror.IsPrecondition, internalerror.KindPrecondition},
		{"IsInternal", internalerror.IsInternal, internalerror.KindInternal},
	}

	for _, c := range constructors() {
		for _, p := range predicates {
			t.Run(c.name+"/"+p.name, func(t *testing.T) {
				want := c.kind == p.kind

				for shape, err := range map[string]error{
					"bare":      c.new("classified", nil),
					"with kind": c.new("classified", errCause),
					"wrapped":   fmt.Errorf("transport: %w", c.new("classified", errCause)),
				} {
					if got := p.fn(err); got != want {
						t.Errorf("%s(%s) = %t, want %t", p.name, shape, got, want)
					}
				}
			})
		}
	}
}

func TestKindOfUnclassified(t *testing.T) {
	t.Parallel()

	unclassified := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain error", errCause},
		{"wrapped plain error", fmt.Errorf("open index: %w", errCause)},
	}

	for _, u := range unclassified {
		t.Run(u.name, func(t *testing.T) {
			if got := internalerror.KindOf(u.err); got != internalerror.KindUnclassified {
				t.Errorf("KindOf() = %v, want %v", got, internalerror.KindUnclassified)
			}
			for name, fn := range map[string]func(error) bool{
				"IsBadRequest":   internalerror.IsBadRequest,
				"IsNotFound":     internalerror.IsNotFound,
				"IsPrecondition": internalerror.IsPrecondition,
				"IsInternal":     internalerror.IsInternal,
			} {
				if fn(u.err) {
					t.Errorf("%s() = true, want false", name)
				}
			}
		})
	}
}

func TestKindOfReturnsOutermostClassification(t *testing.T) {
	t.Parallel()

	inner := internalerror.NewInternalError("open index", errCause)
	outer := internalerror.NewPreconditionError("no repositories registered", inner)

	if got := internalerror.KindOf(outer); got != internalerror.KindPrecondition {
		t.Fatalf("KindOf() = %v, want %v", got, internalerror.KindPrecondition)
	}
	if !errors.Is(outer, errCause) {
		t.Error("errors.Is lost the root cause across two classified layers")
	}
}

func TestKindString(t *testing.T) {
	t.Parallel()

	names := map[internalerror.Kind]string{
		internalerror.KindUnclassified: "unclassified",
		internalerror.KindBadRequest:   "bad_request",
		internalerror.KindNotFound:     "not_found",
		internalerror.KindPrecondition: "precondition",
		internalerror.KindInternal:     "internal",
		internalerror.Kind(99):         "unclassified",
	}

	for kind, want := range names {
		if got := kind.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}
}
