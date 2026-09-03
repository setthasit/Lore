package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/services"
)

type whyFlags struct {
	repo string
	out  evidenceOutput
}

func newWhyCommand(resolve Resolver, configPath *string) *cobra.Command {
	var flags whyFlags

	cmd := &cobra.Command{
		Use:   `why <file>:<L1>[-<L2>] ["question"]`,
		Short: "Print why a span of code is the way it is, as a timeline",
		Long: "Blames the given lines of <file> in a registered local clone and prints the\n" +
			"decision trail behind them in the order it happened, each entry with its\n" +
			"source URL. Write a single line as <file>:<L1>, and name the clone with\n" +
			"--repo when the workspace registers more than one. A workspace that\n" +
			"registers no clone cannot anchor on code at all: ask `lore ask` there\n" +
			"instead; --explain answers from the trail in prose, and --raw emits the\n" +
			"evidence bundle as JSON for scripting.",
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			span, err := parseLineSpan(args[0])
			if err != nil {
				return err
			}

			var question string
			if len(args) > 1 {
				question = args[1]
			}

			return withRuntime(cmd, resolve, *configPath, func(rt *Runtime) error {
				bundle, err := rt.Why.Why(cmd.Context(), services.WhyRequest{
					Repo:      flags.repo,
					File:      span.file,
					LineStart: span.start,
					LineEnd:   span.end,
					Question:  question,
				})
				if err != nil {
					return err
				}

				return flags.out.emit(cmd, rt.Synthesis, bundle)
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&flags.repo, "repo", "",
		"the registered clone the file belongs to, by remote (github:owner/repo) or by path; omit it when only one is registered")
	flags.out.flags(cmd)
	return cmd
}

type lineSpan struct {
	file  string
	start int
	end   int
}

var errZeroSpanEnd = errors.New("line span ends at zero")

// A path may itself contain colons, so the span is the part after the last one.
func parseLineSpan(arg string) (lineSpan, error) {
	cut := strings.LastIndex(arg, ":")
	if cut < 0 || cut == len(arg)-1 {
		return lineSpan{}, spanArgError(arg, "names no line span")
	}
	if cut == 0 {
		return lineSpan{}, spanArgError(arg, "names no file")
	}

	span, err := spanBounds(arg[cut+1:])
	switch {
	case errors.Is(err, errZeroSpanEnd):
		return lineSpan{}, spanArgError(arg, "ends its line span at line zero")
	case err != nil:
		return lineSpan{}, spanArgError(arg, "does not number its line span")
	}
	if span.end != 0 && span.end < span.start {
		return lineSpan{}, spanArgError(arg, "ends its line span before it starts")
	}
	span.file = arg[:cut]

	return span, nil
}

func spanBounds(span string) (lineSpan, error) {
	first, last, ranged := strings.Cut(span, "-")
	start, err := strconv.Atoi(first)
	if err != nil {
		return lineSpan{}, err
	}
	if !ranged {
		return lineSpan{start: start}, nil
	}

	end, err := strconv.Atoi(last)
	if err != nil {
		return lineSpan{}, err
	}
	// Zero is the sentinel for an omitted end, so it must not survive the written form.
	if end == 0 {
		return lineSpan{}, errZeroSpanEnd
	}

	return lineSpan{start: start, end: end}, nil
}

func spanArgError(arg, fault string) error {
	return internalerror.NewBadRequestError(
		fmt.Sprintf("argument %q %s: write it as <file>:<line> or <file>:<first>-<last>", arg, fault), nil)
}
