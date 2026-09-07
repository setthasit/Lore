package plugexec

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
)

// fixturePath is the Python plugin, which lives outside this package because it
// is not Go and because `lore plugin verify` has to be able to point at it the
// way it points at any third-party binary.
const fixturePath = "../../test/fixtures/plugins/pysource.py"

// pythonPlugin returns an executable that runs the fixture. The host executes a
// binary with no arguments and no inherited environment, so the interpreter and
// the script are baked into a launcher rather than passed: `#!/usr/bin/env
// python3` would need a PATH the child deliberately does not get.
//
// Absent python3 the test skips; present but misbehaving, it fails.
func pythonPlugin(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a POSIX shell script; the fixture itself is portable")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH, so the cross-language fixture cannot run")
	}
	fixture, err := filepath.Abs(fixturePath)
	if err != nil {
		t.Fatalf("resolving the fixture: %v", err)
	}
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("the fixture plugin is missing: %v", err)
	}

	launcher := filepath.Join(t.TempDir(), "pysource")
	body := "#!/bin/sh\nexec " + quote(python) + " " + quote(fixture) + "\n"
	if err := os.WriteFile(launcher, []byte(body), 0o700); err != nil {
		t.Fatalf("writing the launcher: %v", err)
	}
	return launcher
}

func quote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// pythonSource opens the fixture and builds one instance of it. cfg is the
// `with:` block as JSON, which is how the fixture is told to crash.
func pythonSource(t *testing.T, binary, config string) lore.Connector {
	t.Helper()
	plugin, err := open(binary, testHost(nil), testTuning())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	source, ok := plugin.(lore.SourcePlugin)
	if !ok {
		t.Fatalf("the fixture opened as %T, want a lore.SourcePlugin", plugin)
	}
	conn, err := source.NewSource(lore.SourceConfig{
		Instance: "pysource",
		Config:   []byte(config),
		Host:     testHost(nil),
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return conn
}

// One suite certifies compiled and external plugins identically: this is the
// suite plugins/sources/* run, reached over a pipe.
func TestPythonFixturePassesTheConformanceSuite(t *testing.T) {
	binary := pythonPlugin(t)

	plugin, err := open(binary, testHost(nil), testTuning())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if manifest := plugin.Manifest(); manifest.Name != "pysource" || manifest.Kind != lore.KindSource {
		t.Errorf("manifest = %+v, want the source plugin pysource", manifest)
	}

	conform.Run(t, func() lore.Connector { return pythonSource(t, binary, `{}`) }, conform.Fixture{Docs: 7})
}

// What `lore plugin verify` runs: no fixture facts, because a host verifying a
// stranger's binary knows neither its document count nor its stream's shape.
func TestPythonFixturePassesTheHostSideCheck(t *testing.T) {
	binary := pythonPlugin(t)

	findings := conform.Check(func() lore.Connector { return pythonSource(t, binary, `{}`) }, conform.Fixture{})
	for _, f := range findings {
		t.Errorf("%s: %s", f.Check, f.Detail)
	}
}

func TestAPluginKilledMidStreamResumesWithoutDuplicates(t *testing.T) {
	binary := pythonPlugin(t)

	// The fixture crashes after its second batch, so the third batch onward was
	// never sent and the second batch's cursor is the last persisted one.
	crashed := pythonSource(t, binary, `{"crash_after_batch":2}`)
	committed, err := drain(crashed, nil)
	if err == nil {
		t.Fatal("the crashing fixture ended its stream cleanly")
	}
	var crash *crashError
	if !errors.As(err, &crash) {
		t.Fatalf("error = %v (%T), want a *crashError", err, err)
	}
	if crash.instance != "pysource" || crash.op != opChanges {
		t.Errorf("crash = %+v, want instance pysource and op changes", crash)
	}
	if len(committed) != 2 {
		t.Fatalf("committed %d batches before the crash, want 2", len(committed))
	}

	// Only what was committed is durable: the host makes no assumption that a
	// killed plugin flushed anything.
	cursor := committed[len(committed)-1].Cursor
	if len(cursor) == 0 {
		t.Fatal("the last committed batch carries no cursor, so there is nothing to resume from")
	}

	resumed, err := drain(pythonSource(t, binary, `{}`), cursor)
	if err != nil {
		t.Fatalf("resuming from %v: %v", cursor, err)
	}

	seen := map[lore.DocID]int{}
	for _, batch := range append(committed, resumed...) {
		for _, doc := range batch.Docs {
			seen[doc.ID]++
		}
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("%s was ingested %d times across the crash boundary", id, count)
		}
	}
	if len(seen) != 7 {
		t.Errorf("the crash and the resume together yielded %d distinct documents, want the whole source's 7", len(seen))
	}
	for i := 1; i <= 7; i++ {
		id := lore.NewDocID("pysource", lore.DocTypeTicket, "doc-"+strconv.Itoa(i))
		if seen[id] == 0 {
			t.Errorf("%s is lost: neither the crashed stream nor the resumed one yielded it", id)
		}
	}
}
