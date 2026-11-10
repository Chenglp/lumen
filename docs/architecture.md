# Architecture

A short account of the decisions that shape everything else. No code yet — this is the map the implementation follows.

## Shape

```
        ┌──────────────┐
        │    server    │  Go, single binary, runs on the machine that holds the files
        │              │  scan · index · metadata · stream · jobs
        └──────┬───────┘
               │ HTTP + one event stream
     ┌─────────┼──────────┬─────────────┐
  desktop    tablet      TV          phone
  (Tauri)   (SwiftUI)  (SwiftUI)    (SwiftUI)
```

There is no cloud tier and no account service. The server is the only stateful component; clients are disposable.

## Playback: decide once, at play time

Clients ask one question — *can this device open this file as-is?* — and the server answers with a mode, not a verdict:

- **direct** — stream the original bytes
- **remux** — repackage into a client-safe container without touching the streams
- **transcode** — encode, queued behind the host's real capacity

Two rules keep this honest:

**Facts decide how to start; guesses only pick a fallback.** Container support is a fact — it can be checked before the first byte. Hardware decoders, HDR tone mapping and audio routing are probabilistic: a device may claim support and still fail. So those never gate playback; they only inform what to offer *after* something broke.

**Remux is not free, but it is not transcode.** It shares the encoder process pool's limits while using no encoding capacity, and it must emit segmented output — a progressive stream without an index makes the progress bar unrunnable.

## The host is whatever the user has

A NAS and a gaming desktop differ by more than an order of magnitude in encoding throughput. Nothing about that is hardcoded:

- At startup the server runs a short measured encode (not a capability list — a real one-second encode) to decide whether hardware encoding actually works
- Throughput is bucketed into a small number of tiers that set concurrency slots
- Estimated start time shown to the user is computed from the measured rate for *this* machine, per target resolution

A machine without hardware encoding is not a broken machine; it just says a bigger number.

## Files, not a library transfer

Media stays where it is — a NAS share, an external drive, a plain folder on a desktop. That means:

- **No absolute paths are stored.** A file is identified by volume identity plus a relative path, so remounting a share at a different letter or mount point does not orphan the record.
- **Paths are normalized before comparison.** Unicode NFC and case folding, computed with proper case folding rather than a naive lowercase — on some file systems several spellings collapse to one file, and without this the same file appears twice after a platform change.
- **The database never lives inside a media folder.** A media folder may be a network share, and a WAL-mode SQLite file on a network share can corrupt silently.
- **File watching is an optimization only.** Events are a hint to rescan a directory, never the source of truth; some platforms simply don't deliver them for network volumes.

## Data

One SQLite file, two handles: a single writer (serialized, one connection) and concurrent readers.

The writer's transactions are kept small and bounded. Long scans happen in batches, with file probing done *outside* the write lock — the failure mode to avoid is a progress update stalling behind a multi-second scan commit, which is felt as the UI freezing even though the scan is healthy.

## Jobs

Scanning, metadata fetching, thumbnail generation and transcoding all compete for the same disk and CPU, so only one heavy job runs at a time; the rest queue, with support for pinning one to the front.

Transcoding is stricter than the rest: a fixed small number of concurrent sessions, and admission control with a wait limit. Past that limit the honest answer is "download it instead" — not a queue position that never advances.

## Events

Clients hold one long-lived stream carrying task and device state. It opens with a full snapshot and then sends deltas; unknown event types must be ignored rather than treated as errors, so the server can add events without breaking old clients.

## Access

Devices are paired once and receive a token; the server identifies callers by that token and never by a device id claimed in a request body.

Permissions are three separate capabilities — play, manage the library, manage devices — that do not imply each other. A TV that can play everything should not be able to delete anything, and that distinction has to exist in the data model rather than in the UI.

## Metadata

TMDB is optional and never on the critical path: a file without metadata is still a playable file, shown by its filename. Metadata is cached locally (checked by id, not by re-searching), the cache expires well within the six-month limit TMDB's terms set, and it can be purged from settings.
