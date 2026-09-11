#!/usr/bin/env python3
"""A Lore source plugin in Python, speaking the protocol in
docs/v3/09-plugin-protocol.md over stdin and stdout.

It exists to prove the protocol is implementable without the Go SDK and that
one conformance suite certifies a compiled and an external plugin identically,
so it stays a single file with no third-party imports and does exactly what the
document says: NDJSON in, NDJSON out, one in-flight request, a cursor on every
batch, and stderr as the only diagnostic channel.

Its stream is deterministic: seven tickets in batches of two, oldest first,
each batch carrying the index of its last document as the cursor. Replaying
from any batch's cursor yields the documents after it and nothing before it,
and every document keeps its id across runs, so re-ingest is idempotent.

Configuration (the `with:` block):

    crash_after_batch: N   exit non-zero after emitting N batches, mid-stream,
                           to exercise the host's crash-and-resume path.

Nothing here reads the environment: secrets arrive inside the request payload,
so a plugin that consulted os.environ would be asking for something the host
deliberately did not give it. `os` is imported only to leave abruptly.
"""

import json
import os
import sys

API_VERSION = 1
NAME = "pysource"
BATCH_SIZE = 2
DOCUMENTS = 7

MANIFEST = {
    "name": NAME,
    "kind": "source",
    "api_version": API_VERSION,
    "summary": "Deterministic Python fixture source; created_at is the ticket's own creation day",
    "capabilities": {"embed": False, "complete": False, "repo_remotes": False},
    "fields": [
        {
            "name": "crash_after_batch",
            "type": "int",
            "required": False,
            "doc": "exit non-zero after this many batches, to exercise crash-safe resume",
        }
    ],
    "secrets": [],
}


def log(message):
    """stderr is free-form and the host forwards it at debug level."""
    print(f"{NAME}: {message}", file=sys.stderr, flush=True)


def emit(frame):
    """One JSON object per line, flushed, so the host's pipe read returns a
    whole frame rather than half of one."""
    sys.stdout.write(json.dumps(frame, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def document(instance, index):
    """Documents are a pure function of their index, which is what makes a
    replay produce the same ids and the host's upserts idempotent."""
    day = 10 + index
    created = f"2026-08-{day:02d}T09:00:00+02:00"
    updated = f"2026-08-{day:02d}T17:30:00+02:00"
    return {
        "id": f"{instance}:ticket:doc-{index}",
        "source": instance,
        "type": "ticket",
        "repo_ref": "",
        "title": f"Ticket {index}",
        "body": f"Decision {index}: chose the boring option because it is reversible.",
        "author": "ada@example.com",
        "url": f"https://tickets.example.test/{index}",
        "created_at": created,
        "updated_at": updated,
        "refs": [{"kind": "ticket_key", "value": f"PROJ-{index}"}],
    }


def resume_from(cursor):
    """The cursor is this plugin's own shape and opaque to the host: the index
    of the last document it committed. An absent or unreadable cursor is a full
    backfill, which is also what an empty cursor object means."""
    if not isinstance(cursor, dict):
        return 0
    try:
        return int(cursor.get("after", 0))
    except (TypeError, ValueError):
        return 0


def crash_after(config):
    if not isinstance(config, dict):
        return 0
    try:
        return int(config.get("crash_after_batch", 0))
    except (TypeError, ValueError):
        return 0


def changes(request):
    instance = request.get("instance") or NAME
    after = resume_from(request.get("cursor"))
    limit = crash_after(request.get("config"))

    batches = 0
    index = after + 1
    while index <= DOCUMENTS:
        docs = [document(instance, i) for i in range(index, min(index + BATCH_SIZE, DOCUMENTS + 1))]
        index += len(docs)
        batches += 1

        # The cursor travels with the batch it belongs to, never deferred to
        # done: the host commits the documents and then persists this cursor,
        # so a crash below loses only the batches that were never sent.
        emit(
            {
                "v": API_VERSION,
                "id": request["id"],
                "batch": {"docs": docs, "cursor": {"after": str(index - 1)}},
            }
        )
        log(f"sent batch {batches} up to doc-{index - 1}")

        if limit and batches >= limit:
            log(f"crashing after batch {batches} on request")
            sys.stdout.flush()
            # os._exit skips interpreter cleanup, which is the point: a real
            # crash flushes nothing the host can rely on.
            os._exit(9)

    emit({"v": API_VERSION, "id": request["id"], "done": True})


def error(request, kind, message, retryable=False):
    emit(
        {
            "v": API_VERSION,
            "id": request.get("id", ""),
            "error": {"message": message, "retryable": retryable, "kind": kind},
        }
    )


def handle(request):
    """Unknown request fields are ignored, because the protocol evolves
    additively and ignoring is what makes that safe."""
    if request.get("v") != API_VERSION:
        error(
            request,
            "invalid_config",
            f"plugin speaks api_version {API_VERSION}, host speaks {request.get('v')}",
        )
        return True

    op = request.get("op")
    if op == "manifest":
        emit({"v": API_VERSION, "id": request["id"], "ok": True, "manifest": MANIFEST})
    elif op == "changes":
        changes(request)
    elif op == "shutdown":
        emit({"v": API_VERSION, "id": request["id"], "ok": True})
        return False
    else:
        error(request, "not_found", f"{NAME} implements no op {op!r}")
    return True


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        if not handle(json.loads(line)):
            break
    # Reaching here means shutdown was answered or stdin hit EOF, and EOF is
    # the host's cancel: abandon everything in flight and exit cleanly.
    sys.stdout.flush()
    return 0


if __name__ == "__main__":
    sys.exit(main())
