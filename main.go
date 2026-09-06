// Command remap-player is a standalone recovery tool for
// ark-cloud-storage-no-overflow. If a player's Unreal Engine PlayerId
// changes (e.g. after restoring a corrupted ARK world save to an earlier
// backup, or starting a fresh character after losing access to an old
// one), their previously-synced DedicatedStorage rows become permanently
// orphaned under the old PlayerId — the storage data survives, but
// nothing can address it anymore. This tool remaps
// DedicatedStorage.ownerId from an old value to a new one.
//
// Built as a standalone Go binary using a pure-Go SQLite driver
// (modernc.org/sqlite — no cgo, no native compilation, no .node/.dll
// binary at all) specifically to avoid the entire native-addon-ABI bug
// class documented in CLAUDE.md's SqliteResilienceService section. This
// tool is run rarely and by hand, so there's no reason to carry that risk
// for it even though the underlying app itself no longer does either
// (having migrated to @prisma/adapter-libsql for the same reason).
//
// Usage:
//   clouddb-remap-player.exe                                    (fully interactive)
//   clouddb-remap-player.exe -db path\to\cloudstorage.db
//   clouddb-remap-player.exe -db path\to\cloudstorage.db -old 123456 -new 789012
package main

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	code := run()
	fmt.Println()
	fmt.Println("Press Enter to close this window...")
	bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(code)
}

func run() int {
	dbPathFlag := flag.String("db", "", "Path to cloudstorage.db (prompted if omitted)")
	oldIDFlag := flag.String("old", "", "Old PlayerId to migrate FROM (prompted if omitted)")
	newIDFlag := flag.String("new", "", "New PlayerId to migrate TO (prompted if omitted)")
	flag.Parse()

	reader := bufio.NewReader(os.Stdin)

	dbPath := *dbPathFlag
	if dbPath == "" {
		dbPath = promptForDBPath(reader)
	}

	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return fail("Could not resolve database path: %v", err)
	}
	dbPath = absPath

	if info, err := os.Stat(dbPath); err != nil {
		return fail("Database file not found at %s: %v", dbPath, err)
	} else if info.IsDir() {
		resolved, err := resolveDBPathFromFolder(dbPath, reader)
		if err != nil {
			return fail("%v", err)
		}
		dbPath = resolved
	}

	fmt.Println()
	fmt.Println("IMPORTANT: make sure the Cloud Storage server/launcher is NOT")
	fmt.Println("running right now. Running this tool while the app is writing")
	fmt.Println("to the same database file at the same time can corrupt it.")
	fmt.Print("Type YES to confirm the app is stopped and continue: ")
	if !readYes(reader) {
		fmt.Println("Aborted — nothing was changed.")
		return 0
	}

	backupPath, err := backupDatabase(dbPath)
	if err != nil {
		return fail("Failed to create a backup before proceeding: %v", err)
	}
	fmt.Printf("Backup created: %s\n", backupPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fail("Failed to open database: %v", err)
	}
	defer db.Close()

	if err := printOwnerSummary(db); err != nil {
		return fail("Failed to read DedicatedStorage: %v", err)
	}

	oldID := *oldIDFlag
	if oldID == "" {
		oldID = promptForID(reader, "Enter the OLD PlayerId (the one storage is currently under)")
	}
	newID := *newIDFlag
	if newID == "" {
		newID = promptForID(reader, "Enter the NEW PlayerId (the one to move storage to)")
	}

	if oldID == newID {
		fmt.Println("Old and new PlayerId are the same — nothing to do.")
		return 0
	}

	count, err := countRows(db, oldID)
	if err != nil {
		return fail("Failed to check existing rows for %s: %v", oldID, err)
	}
	if count == 0 {
		fmt.Printf("No DedicatedStorage rows found for PlayerId %s — nothing to migrate.\n", oldID)
		return 0
	}

	fmt.Printf("\nAbout to migrate %d row(s) from PlayerId %s to %s.\n", count, oldID, newID)
	fmt.Println("Rows that collide on the same (clusterId, resourceId) under the new")
	fmt.Println("PlayerId will have their amounts SUMMED, not overwritten.")
	fmt.Print("Type YES to proceed: ")
	if !readYes(reader) {
		fmt.Println("Aborted — no changes made (your backup is still there if needed).")
		return 0
	}

	merged, moved, err := remapOwner(db, oldID, newID)
	if err != nil {
		return fail("Migration failed: %v\nYour original data is safe in the backup: %s", err, backupPath)
	}

	fmt.Println()
	fmt.Printf("Done. %d row(s) merged into existing entries, %d row(s) moved directly.\n", merged, moved)
	fmt.Println("If anything looks wrong, restore from the backup file listed above.")
	return 0
}

func readYes(reader *bufio.Reader) bool {
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line) == "YES"
}

// resolveDBPathFromFolder handles the very likely case that a player
// pointed this tool at a folder (their own working directory, or the
// launcher's install root) instead of the exact cloudstorage.db file
// path — a natural assumption for someone who thinks in terms of "where
// the app lives" rather than the exact file inside it. Rather than
// rejecting this outright, searches the handful of locations
// clouddbGoLauncher/clouddbGo actually use in practice, and only falls
// back to asking the player to type an exact path if none of those match.
func resolveDBPathFromFolder(folder string, reader *bufio.Reader) (string, error) {
	candidates := []string{
		filepath.Join(folder, "cloudstorage.db"),
		filepath.Join(folder, "data", "cloudstorage.db"),
		filepath.Join(folder, "app", "data", "cloudstorage.db"),
	}

	var found []string
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			found = append(found, c)
		}
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf(
			"%s is a folder, not the database file itself, and no "+
				"cloudstorage.db was found in any of the usual spots "+
				"under it:\n"+
				"  %s\n"+
				"  %s\n"+
				"  %s\n"+
				"Run this tool again with the exact path to the real "+
				"cloudstorage.db file.",
			folder, candidates[0], candidates[1], candidates[2],
		)
	case 1:
		fmt.Printf("Found cloudstorage.db at: %s\n", found[0])
		return found[0], nil
	default:
		fmt.Println("Found more than one cloudstorage.db under that folder:")
		for i, f := range found {
			fmt.Printf("  [%d] %s\n", i+1, f)
		}
		for {
			fmt.Print("Which one? Enter a number: ")
			line, _ := reader.ReadString('\n')
			n, err := strconv.Atoi(strings.TrimSpace(line))
			if err == nil && n >= 1 && n <= len(found) {
				return found[n-1], nil
			}
			fmt.Println("  Not a valid choice — try again.")
		}
	}
}

func promptForDBPath(reader *bufio.Reader) string {
	exePath, err := os.Executable()
	defaultPath := "data\\cloudstorage.db"
	if err == nil {
		defaultPath = filepath.Join(filepath.Dir(exePath), "data", "cloudstorage.db")
	}

	fmt.Printf("Path to cloudstorage.db [%s]: ", defaultPath)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultPath
	}
	return line
}

func promptForID(reader *bufio.Reader, label string) string {
	for {
		fmt.Printf("%s: ", label)
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if _, err := strconv.ParseInt(line, 10, 64); err == nil {
			return line
		}
		fmt.Println("  That doesn't look like a numeric PlayerId — try again.")
	}
}

// backupDatabase makes an unconditional plain file copy before any change
// is made — no flag to skip this. Not using VACUUM INTO here (unlike
// SqliteResilienceService's own backup mechanism) since this tool doesn't
// hold a live connection yet at this point and a plain copy of a file the
// app itself isn't currently writing to (per the confirmation prompt
// above) is sufficient for a one-off manual recovery tool.
func backupDatabase(dbPath string) (string, error) {
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	backupPath := fmt.Sprintf("%s.pre-remap.%s.bak", dbPath, timestamp)

	src, err := os.Open(dbPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	dst, err := os.Create(backupPath)
	if err != nil {
		return "", err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}
	return backupPath, nil
}

func printOwnerSummary(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT ownerId, COUNT(*) AS rowCount, SUM(amount) AS totalItems
		FROM DedicatedStorage
		GROUP BY ownerId
		ORDER BY totalItems DESC
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Println()
	fmt.Println("Current PlayerIds with stored inventory:")
	fmt.Println("  PlayerId              Rows      Total Items")
	fmt.Println("  --------------------  --------  -----------")
	found := false
	for rows.Next() {
		var ownerID string
		var rowCount, totalItems int64
		if err := rows.Scan(&ownerID, &rowCount, &totalItems); err != nil {
			return err
		}
		found = true
		fmt.Printf("  %-20s  %-8d  %d\n", ownerID, rowCount, totalItems)
	}
	if !found {
		fmt.Println("  (no rows found)")
	}
	fmt.Println()
	return rows.Err()
}

func countRows(db *sql.DB, ownerID string) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM DedicatedStorage WHERE ownerId = ?`, ownerID).Scan(&count)
	return count, err
}

// remapOwner moves every DedicatedStorage row from oldID to newID.
//
// Rows that collide on (clusterId, resourceId) under newID — the expected,
// common case for a player recovering access on a server they were
// already active on, not an edge case: clusterId is unchanged and
// resourceId is drawn from ARK's fixed set of resource types, so overlap
// between old and new rows is likely — have their amounts SUMMED into the
// existing newID row, and the oldID row is then deleted. Rows with no
// collision are moved directly via a plain UPDATE. The whole operation
// runs inside one transaction, so a failure partway through leaves the
// database in its original state rather than a half-migrated one.
func remapOwner(db *sql.DB, oldID, newID string) (merged int, moved int, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// Step 1: for every (clusterId, resourceId) that exists under BOTH
	// oldID and newID, add oldID's amount into newID's existing row.
	mergeResult, err := tx.Exec(`
		UPDATE DedicatedStorage AS newRow
		SET amount = newRow.amount + (
			SELECT oldRow.amount FROM DedicatedStorage AS oldRow
			WHERE oldRow.ownerId = ?
			  AND oldRow.clusterId = newRow.clusterId
			  AND oldRow.resourceId = newRow.resourceId
		)
		WHERE newRow.ownerId = ?
		  AND EXISTS (
			SELECT 1 FROM DedicatedStorage AS oldRow
			WHERE oldRow.ownerId = ?
			  AND oldRow.clusterId = newRow.clusterId
			  AND oldRow.resourceId = newRow.resourceId
		  )
	`, oldID, newID, oldID)
	if err != nil {
		return 0, 0, fmt.Errorf("merging colliding rows: %w", err)
	}
	mergedCount, _ := mergeResult.RowsAffected()

	// Step 2: remove the now-redundant oldID rows that were just merged in,
	// so step 3's blind UPDATE below never touches them.
	if _, err = tx.Exec(`
		DELETE FROM DedicatedStorage
		WHERE ownerId = ?
		  AND EXISTS (
			SELECT 1 FROM DedicatedStorage AS newRow
			WHERE newRow.ownerId = ?
			  AND newRow.clusterId = DedicatedStorage.clusterId
			  AND newRow.resourceId = DedicatedStorage.resourceId
		  )
	`, oldID, newID); err != nil {
		return 0, 0, fmt.Errorf("removing merged old rows: %w", err)
	}

	// Step 3: everything remaining under oldID had no collision — safe to
	// move directly without violating the (clusterId, ownerId, resourceId)
	// primary key.
	moveResult, err := tx.Exec(`
		UPDATE DedicatedStorage SET ownerId = ? WHERE ownerId = ?
	`, newID, oldID)
	if err != nil {
		return 0, 0, fmt.Errorf("moving non-colliding rows: %w", err)
	}
	movedCount, _ := moveResult.RowsAffected()

	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("committing transaction: %w", err)
	}

	return int(mergedCount), int(movedCount), nil
}

func fail(format string, args ...interface{}) int {
	fmt.Fprintf(os.Stderr, "\nERROR: "+format+"\n", args...)
	return 1
}
