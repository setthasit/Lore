package secrets_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/secrets"
)

func TestWriterScrubsRenderedLog(t *testing.T) {
	t.Parallel()

	const (
		plain  = "fake-secret-value-0000"
		quoted = `fake"quoted-secret`
	)
	cases := map[string]func(sink *secrets.Sink, log *slog.Logger){
		"as the message": func(sink *secrets.Sink, log *slog.Logger) {
			sink.Record("token", plain)
			log.Info(plain)
		},
		"as an attribute value": func(sink *secrets.Sink, log *slog.Logger) {
			sink.Record("token", plain)
			log.Info("sync", "token", plain)
		},
		"as a key": func(sink *secrets.Sink, log *slog.Logger) {
			sink.Record("token", plain)
			log.Info("sync", plain, "on")
		},
		"inside an error attr": func(sink *secrets.Sink, log *slog.Logger) {
			sink.Record("token", plain)
			log.Error("sync failed", "err", fmt.Errorf("auth %s rejected", plain))
		},
		"in a With attr attached before recording": func(sink *secrets.Sink, log *slog.Logger) {
			log = log.With("token", plain)
			sink.Record("token", plain)
			log.Info("sync")
		},
		"escaped by the handler's quoting": func(sink *secrets.Sink, log *slog.Logger) {
			sink.Record("token", quoted)
			log.Info("sync", "token", quoted)
		},
		"quoted by %q inside an error attr": func(sink *secrets.Sink, log *slog.Logger) {
			sink.Record("token", quoted)
			log.Error("sync failed", "err", fmt.Errorf("auth %q rejected", quoted))
		},
	}

	for name, emit := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			sink := &secrets.Sink{}
			emit(sink, slog.New(slog.NewTextHandler(sink.Writer(&buf), nil)))

			line := buf.String()
			if strings.Contains(line, "fake") {
				t.Errorf("line leaks the value: %q", line)
			}
			if !strings.Contains(line, secrets.Placeholder) {
				t.Errorf("line lacks %q: %q", secrets.Placeholder, line)
			}
		})
	}

	t.Run("a line without a recorded value is untouched", func(t *testing.T) {
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
	})
}
