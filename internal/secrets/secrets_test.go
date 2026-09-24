package secrets_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/secrets"
)

const plain = "fake-secret-value-0000"

func TestWriterScrubsRenderedLog(t *testing.T) {
	t.Parallel()

	const quoted = `fake"quoted\secret`
	cases := map[string]struct {
		emit func(sink *secrets.Sink, log *slog.Logger)
		want string
	}{
		"as the message": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("token", plain)
				log.Info(plain)
			},
			want: "level=INFO msg=[redacted]",
		},
		"as an attribute value": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("token", plain)
				log.Info("sync", "token", plain)
			},
			want: "level=INFO msg=sync token=[redacted]",
		},
		"as a key": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("token", plain)
				log.Info("sync", plain, "on")
			},
			want: "level=INFO msg=sync [redacted]=on",
		},
		"as a group name": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("token", plain)
				log.Info("sync", slog.Group(plain, "id", 7))
			},
			want: "level=INFO msg=sync [redacted].id=7",
		},
		"inside an error attr": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("token", plain)
				log.Error("sync failed", "err", fmt.Errorf("auth %s rejected", plain))
			},
			want: `level=ERROR msg="sync failed" err="auth [redacted] rejected"`,
		},
		"in a With attr attached before recording, on a logger that already logged": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				log = log.With("token", plain)
				log.Info("warm")
				sink.Record("token", plain)
				log.Info("sync")
			},
			want: "level=INFO msg=sync token=[redacted]",
		},
		"at every occurrence in the line": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("token", plain)
				log.Info("sync", "a", plain, "b", plain)
			},
			want: "level=INFO msg=sync a=[redacted] b=[redacted]",
		},
		"recorded with surrounding whitespace": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("token", " \t"+plain+"\n")
				log.Info("sync", "token", plain)
			},
			want: "level=INFO msg=sync token=[redacted]",
		},
		"at exactly the minimum length": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("token", "fake1234")
				log.Info("sync", "token", "fake1234")
			},
			want: "level=INFO msg=sync token=[redacted]",
		},
		"escaped by the handler's quoting": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("token", quoted)
				log.Info("sync", "token", quoted)
			},
			want: `level=INFO msg=sync token="[redacted]"`,
		},
		"quoted by %q inside an error attr": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("token", quoted)
				log.Error("sync failed", "err", fmt.Errorf("auth %q rejected", quoted))
			},
			want: `level=ERROR msg="sync failed" err="auth \"[redacted]\" rejected"`,
		},
		"one value contained in another": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("inner", "secret-value")
				sink.Record("token", plain)
				log.Info("sync", "token", plain)
			},
			want: "level=INFO msg=sync token=[redacted]",
		},
		"two values overlapping": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("left", "fake-overlap-one")
				sink.Record("right", "overlap-one-tail")
				log.Info("sync", "token", "fake-overlap-one-tail")
			},
			want: "level=INFO msg=sync token=[redacted]",
		},
		"two values adjacent": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("left", "fake-left-half")
				sink.Record("right", "fake-right-half")
				log.Info("sync", "token", "fake-left-halffake-right-half")
			},
			want: "level=INFO msg=sync token=[redacted]",
		},
		"a value overlapping itself": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("token", "fake-fake-fake")
				log.Info("sync", "token", "fake-fake-fake-fake")
			},
			want: "level=INFO msg=sync token=[redacted]",
		},
		"not when shorter than the minimum, in any form": {
			emit: func(sink *secrets.Sink, log *slog.Logger) {
				sink.Record("pin", "fake123")
				sink.Record("quoted", `fak"e12`)
				log.Info("sync", "pin", "fake123", "quoted", `fak"e12`)
			},
			want: `level=INFO msg=sync pin=fake123 quoted="fak\"e12"`,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			sink := &secrets.Sink{}
			c.emit(sink, slog.New(slog.NewTextHandler(sink.Writer(&buf), nil)))

			lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
			_, got, _ := strings.Cut(lines[len(lines)-1], " ")
			if got != c.want {
				t.Errorf("line after the time = %q, want %q", got, c.want)
			}
		})
	}
}

func TestScrubMatchesEveryFormAnOutputEscapesAValueInto(t *testing.T) {
	t.Parallel()

	const controlAndQuote = "fake\x01tok\"en\tvalue"
	cases := map[string]struct {
		value  string
		render func(t *testing.T, value string) string
	}{
		"rendered inert on one line": {
			value: controlAndQuote,
			render: func(_ *testing.T, value string) string {
				return "title: " + strings.NewReplacer("\x01", `\x01`, "\t", `\t`).Replace(value)
			},
		},
		"rendered inert inside a multi-line excerpt": {
			value: controlAndQuote,
			render: func(_ *testing.T, value string) string {
				return "      " + strings.ReplaceAll(value, "\x01", `\x01`)
			},
		},
		"encoded as JSON": {
			value: "fake&tok<en>\x01value",
			render: func(t *testing.T, value string) string {
				return jsonText(t, map[string]string{"title": value})
			},
		},
		"encoded as JSON without HTML escaping": {
			value: "fake\x01tok\"en&value",
			render: func(t *testing.T, value string) string {
				return jsonTextUnescapedHTML(t, map[string]string{"title": value})
			},
		},
		"quoted by %q inside a JSON-encoded error": {
			value: `fake"tok&en-value`,
			render: func(t *testing.T, value string) string {
				return jsonText(t, map[string]string{"error": fmt.Sprintf("auth %q rejected", value)})
			},
		},
		"quoted by %q inside JSON carried as text inside JSON": {
			value: "fake\x01tok\"en-value",
			render: func(t *testing.T, value string) string {
				structured := jsonText(t, map[string]string{"error": fmt.Sprintf("auth %q rejected", value)})
				return jsonTextUnescapedHTML(t, map[string]any{"text": structured, "structured": json.RawMessage(structured)})
			},
		},
		"quoted by %q in a plugin log line the host logs again": {
			value: `fake"tok\en-value`,
			render: func(t *testing.T, value string) string {
				pluginLine := textLine(t, "sync failed", "err", fmt.Errorf("auth %q rejected", value))
				return textLine(t, "plugin wrote to stderr", "line", pluginLine)
			},
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			sink := &secrets.Sink{}
			sink.Record("token", c.value)
			if got, want := sink.Scrub(c.render(t, c.value)), c.render(t, secrets.Placeholder); got != want {
				t.Errorf("Scrub() = %q, want %q", got, want)
			}
		})
	}
}

func jsonText(t *testing.T, v any) string {
	t.Helper()

	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	return string(encoded)
}

func jsonTextUnescapedHTML(t *testing.T, v any) string {
	t.Helper()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode %v: %v", v, err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func textLine(t *testing.T, msg string, args ...any) string {
	t.Helper()

	var buf bytes.Buffer
	record := slog.NewRecord(time.Time{}, slog.LevelError, msg, 0)
	record.Add(args...)
	if err := slog.NewTextHandler(&buf, nil).Handle(context.Background(), record); err != nil {
		t.Fatalf("handle %q: %v", msg, err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func TestWriterLeavesALineWithoutARecordedValueByteIdentical(t *testing.T) {
	t.Parallel()

	sink := &secrets.Sink{}
	sink.Record("token", plain)
	var scrubbed, bare bytes.Buffer
	record := slog.NewRecord(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), slog.LevelWarn, "sync done", 0)
	record.AddAttrs(slog.String("path", `C:\tmp\a b`), slog.String("note", "say \"hi\"\t="))
	for _, h := range []slog.Handler{
		slog.NewTextHandler(sink.Writer(&scrubbed), nil),
		slog.NewTextHandler(&bare, nil),
	} {
		if err := h.Handle(context.Background(), record); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	if !bytes.Equal(scrubbed.Bytes(), bare.Bytes()) {
		t.Errorf("scrubbed line = %q, want %q", scrubbed.Bytes(), bare.Bytes())
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWriterWrite(t *testing.T) {
	t.Parallel()

	line := []byte("token=" + plain + "\n")

	t.Run("reports the caller's length, not the scrubbed one", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		sink := &secrets.Sink{}
		sink.Record("token", plain)
		n, err := sink.Writer(&buf).Write(line)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if n != len(line) {
			t.Errorf("n = %d, want %d", n, len(line))
		}
		if got, want := buf.String(), "token=[redacted]\n"; got != want {
			t.Errorf("written = %q, want %q", got, want)
		}
	})

	t.Run("reports nothing written when the underlying writer fails", func(t *testing.T) {
		t.Parallel()

		broken := errors.New("fake broken pipe")
		n, err := (&secrets.Sink{}).Writer(failingWriter{broken}).Write(line)
		if !errors.Is(err, broken) {
			t.Errorf("err = %v, want %v", err, broken)
		}
		if n != 0 {
			t.Errorf("n = %d, want 0", n)
		}
	})
}

func TestNotices(t *testing.T) {
	t.Parallel()

	type record struct{ field, value string }
	notice := func(fields string) []string {
		return []string{"secrets shorter than 8 characters are not scrubbed: " + fields}
	}
	cases := map[string]struct {
		records []record
		want    []string
	}{
		"nil when every value is long enough": {
			records: []record{{"token", plain}, {"key", "fake1234"}},
			want:    nil,
		},
		"a short value names its field": {
			records: []record{{"token", plain}, {"pin", "fake123"}},
			want:    notice("pin"),
		},
		"padding does not lengthen a value": {
			records: []record{{"pin", "  fake123\t\n "}},
			want:    notice("pin"),
		},
		"length counts characters, not bytes": {
			records: []record{{"pin", "fäkeñ12"}},
			want:    notice("pin"),
		},
		"an empty field is named (unnamed)": {
			records: []record{{"", "fake123"}},
			want:    notice("(unnamed)"),
		},
		"each field is named once, in recording order": {
			records: []record{{"pin", "fake123"}, {"code", "fake456"}, {"pin", "fake789"}},
			want:    notice("pin, code"),
		},
		"a blank value is ignored": {
			records: []record{{"token", " \t\n"}},
			want:    nil,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			sink := &secrets.Sink{}
			for _, r := range c.records {
				sink.Record(r.field, r.value)
			}
			if got := sink.Notices(); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Notices() = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestNilSink(t *testing.T) {
	t.Parallel()

	var sink *secrets.Sink
	sink.Record("token", plain)

	if got := sink.Scrub(plain); got != plain {
		t.Errorf("Scrub() = %q, want %q", got, plain)
	}
	if got := sink.Notices(); got != nil {
		t.Errorf("Notices() = %#v, want nil", got)
	}
	var buf bytes.Buffer
	if got := sink.Writer(&buf); got != io.Writer(&buf) {
		t.Errorf("Writer() = %#v, want the writer it was given", got)
	}
}

func TestSinkIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	const workers = 16
	var buf bytes.Buffer
	sink := &secrets.Sink{}
	log := slog.New(slog.NewTextHandler(sink.Writer(&buf), nil))

	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			long := fmt.Sprintf("fake-long-secret-%02d", i)
			sink.Record(fmt.Sprintf("pin%02d", i), "fake12")
			sink.Record("token", long)
			sink.Notices()
			log.Info("sync", "token", long)
		})
	}
	wg.Wait()

	out := buf.String()
	if strings.Contains(out, "fake") {
		t.Errorf("output leaks a value:\n%s", out)
	}
	if got := strings.Count(out, "token=[redacted]"); got != workers {
		t.Errorf("scrubbed lines = %d, want %d:\n%s", got, workers, out)
	}
	notices := sink.Notices()
	if len(notices) != 1 {
		t.Fatalf("Notices() = %#v, want one line", notices)
	}
	for i := range workers {
		if field := fmt.Sprintf("pin%02d", i); !strings.Contains(notices[0], field) {
			t.Errorf("notice %q does not name %s", notices[0], field)
		}
	}
}
