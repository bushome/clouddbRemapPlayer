# clouddbRemapPlayer — Build Instructions

Standalone recovery tool: remaps `DedicatedStorage.ownerId` from an old
PlayerId to a new one, for a player who lost access to their original ARK
character (world-save restore, fresh character, etc.) and needs their
cloud-storage inventory reattached to their new PlayerId.

Ships as a single static exe, same "download one file, run it" philosophy
as `clouddbGoLauncher` — no Node, no runtime dependencies, and
(deliberately) **no native SQLite addon at all**: this uses
`modernc.org/sqlite`, a pure-Go driver with no `cgo`/native compilation
step, specifically to avoid the entire class of native-addon-ABI bug
documented in CLAUDE.md's SqliteResilienceService section. A tool this
small and infrequently run has no reason to carry that risk.

**Kept as its own separate tree and compilation, not folded into
`clouddbGoLauncher`** — different dependency graph (this pulls in
`modernc.org/sqlite`; the launcher is stdlib-only), different release
cadence (this ships once and gets handed out only if the orphaned-PlayerId
problem actually comes up, versus the launcher shipping to every player),
and no shared code between them worth a common module.

## 1. Install Go

Same as the launcher — go1.22+ from https://go.dev/dl/, confirm with
`go version`.

## 2. Resolve dependencies

```powershell
cd C:\Dev\clouddbRemapPlayer
go mod tidy
```

**This step is required, not optional** — the `go.mod` in this scaffold
lists the top-level dependency (`modernc.org/sqlite`) but was written
without access to a Go toolchain to actually resolve its full transitive
dependency graph and checksums. `go mod tidy` fills in `go.sum` and any
indirect dependencies for real, against the actual current module registry.
Don't skip this expecting the committed `go.mod` alone to be sufficient.

## 3. Build

```powershell
go build -o clouddb-remap-player.exe .
```

No `sync-payload.cmd`-equivalent step needed — this tool has no embedded
payload, just the one Go source file and its resolved dependencies.

## 4. Test

Test against a **copy** of a real `cloudstorage.db`, never the live file
directly, until you've confirmed the tool behaves as expected:

```powershell
copy path\to\real\cloudstorage.db test-cloudstorage.db
.\clouddb-remap-player.exe -db test-cloudstorage.db
```

Walk through the prompts:
1. Confirms the app isn't running (type `YES`).
2. Creates a `.pre-remap.<timestamp>.bak` copy automatically — verify this
   file actually appears before proceeding.
3. Prints the current PlayerId summary table — verify the numbers look
   right against what you know is really in the test file.
4. Enter an old and new PlayerId. Try this against IDs you know will
   **collide** on at least one `(clusterId, resourceId)` pair (the common,
   expected case per CLAUDE.md's design note) — verify the reported
   merged-vs-moved counts make sense, and manually check the resulting
   database that colliding amounts were actually **summed**, not
   overwritten or duplicated.
5. Confirm a fresh run against the same test file no longer shows the old
   PlayerId in the summary table (everything moved/merged into the new
   one).

Only point it at a real `cloudstorage.db` once you're confident in the
above — and even then, remember the tool's own automatic backup is there
as a safety net regardless.

## Known limitations (by design, not oversights)

- **No process-detection for "is the app currently running."** The tool
  asks the operator to confirm manually rather than trying to detect this
  automatically — reliable cross-process file-lock detection adds real
  complexity for a tool this rarely used, and the manual confirmation
  plus automatic backup are judged sufficient. Worth revisiting if this
  tool ever gets used often enough that "forgot to stop the app first"
  becomes a real recurring failure mode.
- **MySQL not supported.** This targets the SQLite/solo-player case
  specifically (`DatabaseService`'s `SQLite.File` path) — a MySQL cluster
  operator wanting the same remap would need a version pointed at
  `@prisma/adapter-mariadb`'s connection instead. Worth building if this
  need actually comes up on the cluster-operator side; not built
  speculatively for now.
- **Only remaps `DedicatedStorage.ownerId`** — confirmed against the
  schema that no other column/table keys on PlayerId in a way that would
  also need remapping.
