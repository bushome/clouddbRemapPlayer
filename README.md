```

 /$$$$$$$                    /$$       /$$           /$$                          
| $$__  $$                  | $$      |__/          | $$                             ______
| $$  \ $$  /$$$$$$         | $$       /$$ /$$$$$$$ | $$   /$$  /$$$$$$   /$$$$$$   |_,.,--\ 
| $$$$$$$/ /$$__  $$ /$$$$$$| $$      | $$| $$__  $$| $$  /$$/ /$$__  $$ /$$__  $$     ||
| $$__  $$| $$$$$$$$|______/| $$      | $$| $$  \ $$| $$$$$$/ | $$$$$$$$| $$  \__/     ||
| $$  \ $$| $$_____/        | $$      | $$| $$  | $$| $$_  $$ | $$_____/| $$           ##
| $$  | $$|  $$$$$$$        | $$$$$$$$| $$| $$  | $$| $$ \  $$|  $$$$$$$| $$           ##
|__/  |__/ \_______/        |________/|__/|__/  |__/|__/  \__/ \_______/|__/      
```
This page contains the source files for the Player Re-linker tool packaged with the release files for the Cloud
Dedicated Storage No-Overflow API Variant, https://github.com/bushome/ark-cloud-storage-no-overflow/releases, 
built using GoLauncher. 

# clouddbRemapPlayer — Build Instructions

Standalone recovery tool: remaps `DedicatedStorage.ownerId` from an old
PlayerId to a new one, for a player who lost access to their original ARK
character (world-save restore, fresh character, etc.) and needs their
cloud-storage inventory reattached to their new PlayerId.

Supports both backends this project ships — SQLite (solo-player) and
MySQL/MariaDB (cluster operator). Ships as a single static binary per
platform, same "download one file, run it" philosophy as
`clouddbGoLauncher` — no Node, no runtime dependencies, and
(deliberately) **no native database addon at all on either backend**:
this uses `modernc.org/sqlite` and `github.com/go-sql-driver/mysql`, both
pure-Go drivers with no `cgo`/native compilation step, specifically to
avoid the entire class of native-addon-ABI bug documented in CLAUDE.md's
SqliteResilienceService section. A tool this small and infrequently run
has no reason to carry that risk, on either database.

Being pure Go with zero `cgo` on either backend also has a second
benefit: **cross-compiling a native Linux binary needs no extra
toolchain, no CGO, and no separate build machine** — see step 3 below.
This matters because the tool works with any of this project's
deployment targets, including the Linux/Unix `pm2` path, and a `.exe`
alone would leave that audience with no native option.

**Kept as its own separate tree and compilation, not folded into
`clouddbGoLauncher`** — different dependency graph (this pulls in
`modernc.org/sqlite` and `github.com/go-sql-driver/mysql`; the launcher
is stdlib-only), different release cadence (this ships once and gets
handed out only if the orphaned-PlayerId problem actually comes up,
versus the launcher shipping to every player), and no shared code between
them worth a common module.

## 1. Install Go

Same as the launcher — go1.22+ from https://go.dev/dl/, confirm with
`go version`.

## 2. Resolve dependencies

```powershell
cd "C:\Dev\ARK CLOUD DEDICATED STORAGE API - No OverFlow\clouddbRemapPlayer"
go mod tidy
```

**This step is required, not optional** — the `go.mod` in this scaffold
lists the top-level dependencies (`modernc.org/sqlite` and
`github.com/go-sql-driver/mysql`) but was written without access to a Go
toolchain to actually resolve the full transitive dependency graph and
checksums for the MySQL driver. `go mod tidy` fills in `go.sum` and any
indirect dependencies for real, against the actual current module
registry. Don't skip this expecting the committed `go.mod` alone to be
sufficient.

## 3. Build

**Windows** (same as before):
```powershell
go build -o clouddb-remap-player.exe .
```

**Linux** — cross-compiled from the same Windows workstation, no
separate build machine or toolchain needed, since there's no `cgo` on
either platform to complicate cross-compilation:
```powershell
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -o clouddb-remap-player .
Remove-Item Env:\GOOS, Env:\GOARCH
```
(`Remove-Item` resets the environment variables afterward so a later
`go build` in the same PowerShell session doesn't silently keep
targeting Linux.) The `cmd`-equivalent, if needed:
```cmd
set GOOS=linux
set GOARCH=amd64
go build -o clouddb-remap-player .
set GOOS=
set GOARCH=
```

Both binaries build from the identical source — no `#ifdef`-style
platform branches in this tool's code, since everything it does (file
I/O, SQL over `database/sql`) is already cross-platform via the standard
library and the two pure-Go drivers.

No `sync-payload.cmd`-equivalent step needed either way — this tool has
no embedded payload, just the Go source and its resolved dependencies.

## 4. Test

### SQLite path

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

### MySQL/MariaDB path

Test against `arktest_clouddb` (the existing isolated MariaDB sandbox
already used elsewhere in this project) — never a production database —
until you've confirmed the tool behaves as expected:

```powershell
.\clouddb-remap-player.exe -mysql -host <arktest_clouddb host> -port 3306 -user <test user> -password <test password> -database arktest_clouddb
```

Walk through the same shape of prompts, adjusted for this backend:
1. Connects and pings the database — verify a clear, specific error
   appears if you deliberately provide a wrong password or unreachable
   host, rather than a vague failure.
2. Creates a `<database>.pre-remap.<timestamp>.sql` backup script
   automatically — verify this file appears, and spot-check that it
   contains real `INSERT ... ON DUPLICATE KEY UPDATE` rows matching the
   test database's actual `DedicatedStorage` contents before proceeding.
3. Prints the current PlayerId summary table — same verification as the
   SQLite path.
4. Test a collision case the same way — verify the merged/moved counts
   and that colliding amounts were summed, not overwritten. **This
   exercises the `UPDATE ... JOIN`/`DELETE ... JOIN` queries specifically
   used for MySQL** (see `main.go`'s comments on `remapOwnerMySQL` for why
   these differ from the SQLite version's subqueries — MySQL rejects a
   subquery that selects from the table being updated/deleted in the same
   statement, error 1093) — don't assume the SQLite path's test coverage
   validates this logic too; it's genuinely different SQL.
5. Confirm a fresh run no longer shows the old PlayerId.

**Note: there's no "confirm the app isn't running" prompt on this path**
— unlike the SQLite walkthrough above, MySQL mode goes straight from
connecting to taking the backup. This is deliberate, not a step that got
missed: the original wording ("make sure the Cloud Storage server is NOT
running") doesn't fit a networked database, since the MySQL/MariaDB
server itself obviously needs to stay running for this tool to connect
at all — and the underlying concern the prompt existed for (the API also
writing to the same rows) doesn't carry the same physical-corruption
risk a file-based SQLite write collision does.

### Credentials file, both backends

Regardless of which backend you tested above:
1. After a successful connection, confirm the tool offers to save
   connection details, and confirm `remap-player-credentials.json`
   actually appears next to the exe after answering yes.
2. Run the tool again — confirm it skips straight past the
   database-type/connection prompts using the saved file.
3. Delete `remap-player-credentials.json` and run again — confirm it
   prompts fresh, exactly as on a first run.
4. Test `-no-save` on a run with no existing credentials file — confirm
   it completes normally but does *not* create the file afterward.

### Linux binary — verified via WSL2 (2026-09-07)

Confirmed working end-to-end, not just a valid cross-compile: WSL2 was
already present on the workstation from the Docker Desktop setup (see
the main project docs' Docker section), so no separate Linux machine or
VM was needed. From a WSL2 Ubuntu shell:
```bash
cp /mnt/c/path/to/clouddb-remap-player ~/
chmod +x ~/clouddb-remap-player
~/clouddb-remap-player -mysql -host <arktest_clouddb host> -port 3306 -user <test user> -password <test password> -database arktest_clouddb
```
(`chmod +x` is required — copying through the Windows filesystem doesn't
preserve the Linux executable bit.) Produced an identical connect/
backup/summary result to the Windows exe against the same
`arktest_clouddb` instance, confirming the cross-compiled binary isn't
just believed-safe — it's a real, working ELF binary. Note it wrote its
own local `remap-player-credentials.json` separate from the Windows
exe's copy, since it ran from a different working directory — expected
behavior, not a bug, given the "config lives alongside wherever this
runs" convention.

If you don't have WSL2 or another Linux environment handy, at minimum
confirm `file clouddb-remap-player` reports a Linux ELF binary rather
than a Windows PE, so a bad cross-compile flag doesn't ship silently.

Only point any of this at a real production database once you're
confident in the above — and even then, remember the tool's own
automatic backup (file copy for SQLite, SQL script for MySQL) is there as
a safety net regardless.

## Known limitations (by design, not oversights)

- **No process-detection for "is the app currently running."** The tool
  asks the operator to confirm manually rather than trying to detect this
  automatically — reliable cross-process file-lock detection (for
  SQLite) or connection-monitoring (for MySQL) adds real complexity for a
  tool this rarely used, and the manual confirmation plus automatic
  backup are judged sufficient. Worth revisiting if this tool ever gets
  used often enough that "forgot to stop the app first" becomes a real
  recurring failure mode.
- **The MySQL backup is a manual-recovery safety net, not a one-click
  restore.** Unlike SQLite's plain file copy, there's no single file to
  copy for a networked database — the tool instead dumps the entire
  `DedicatedStorage` table as a `.sql` script an operator has to run by
  hand via a MySQL client if a restore is ever needed. Worth being
  upfront about this difference in any user-facing docs, so a MySQL
  operator doesn't expect the same one-step recovery the SQLite backup
  file offers.
- **No special escaping for unusual characters in a MySQL password when
  building the connection string.** The DSN is built with a plain
  `fmt.Sprintf`, not a dedicated DSN-building helper — a password
  containing `@` or `)` could in principle produce a malformed
  connection string. Not handled specially for now, given how rarely this
  tool is run and how easy it is to notice and retype a password if the
  connection fails; worth revisiting only if this actually trips someone
  up in practice.
- **Only remaps `DedicatedStorage.ownerId`** — confirmed against the
  schema (both backends) that no other column/table keys on PlayerId in
  a way that would also need remapping.
