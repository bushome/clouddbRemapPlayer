// Command remap-player is a standalone recovery tool for
// ark-cloud-storage-no-overflow. If a player's Unreal Engine PlayerId
// changes (e.g. after restoring a corrupted ARK world save to an earlier
// backup, or starting a fresh character after losing access to an old
// one), their previously-synced DedicatedStorage rows become permanently
// orphaned under the old PlayerId — the storage data survives, but
// nothing can address it anymore. This tool remaps
// DedicatedStorage.ownerId from an old value to a new one.
//
// Supports both backends this project ships: SQLite (the solo-player
// Go-launcher target) and MySQL/MariaDB (the cluster-operator SEA
// target). Both drivers used here are pure Go — modernc.org/sqlite and
// github.com/go-sql-driver/mysql — no cgo, no native .node/.dll binary
// at all, specifically to avoid the entire native-addon-ABI bug class
// documented in CLAUDE.md's SqliteResilienceService section. This tool
// is run rarely and by hand, so there's no reason to carry that risk for
// it even though the underlying app itself no longer does either (having
// migrated to @prisma/adapter-libsql for the same reason).
//
// Usage:
//   clouddb-remap-player.exe                                    (fully interactive)
//   clouddb-remap-player.exe -db path\to\cloudstorage.db
//   clouddb-remap-player.exe -db path\to\cloudstorage.db -old 123456 -new 789012
//   clouddb-remap-player.exe -mysql
//   clouddb-remap-player.exe -mysql -host db.example.com -port 3306 -user root -password secret -database clouddb
//
// Connection details (whichever backend you use) are saved to
// remap-player-credentials.json next to this exe after your first
// successful run, so you won't be asked again on future runs. Delete
// that file any time you want to be asked again (e.g. after a password
// change) — there's no separate "update credentials" flow, that's the
// whole reset story. Pass -no-save to skip saving for a single run.
package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

func main() {
	code := run()
	fmt.Println()
	fmt.Println("Press Enter to close this window...")
	bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(code)
}

// mysqlCredentials holds everything needed to open a MySQL/MariaDB
// connection.
type mysqlCredentials struct {
	Host     string `json:"Host"`
	Port     int    `json:"Port"`
	User     string `json:"User"`
	Password string `json:"Password"`
	Database string `json:"Database"`
}

// savedCredentials is the on-disk shape of remap-player-credentials.json.
// Only one of SQLitePath/MySQL is meaningful at a time, based on DBType —
// this mirrors config.json's own DTO shape (a SQLite block and a MySQL
// block coexisting, with UseMySQL deciding which one matters) rather than
// inventing a different convention just for this tool.
type savedCredentials struct {
	DBType     string             `json:"DBType"` // "sqlite" or "mysql"
	SQLitePath string             `json:"SQLitePath,omitempty"`
	MySQL      *mysqlCredentials  `json:"MySQL,omitempty"`
}

// credentialsFilePath resolves next to the exe itself, not the current
// working directory — same "config sits alongside the app" convention
// used everywhere else in this project (config.json, watchdog-config.json).
func credentialsFilePath() string {
	exePath, err := os.Executable()
	dir := "."
	if err == nil {
		dir = filepath.Dir(exePath)
	}
	return filepath.Join(dir, "remap-player-credentials.json")
}

func loadSavedCredentials() (*savedCredentials, error) {
	path := credentialsFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var creds savedCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("credentials file at %s is not valid JSON: %w", path, err)
	}
	return &creds, nil
}

func saveCredentials(creds savedCredentials) error {
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	// 0600: local recovery-tool credentials, same trust model as
	// config.json elsewhere in this project — readable only by whoever
	// has filesystem access to this machine already.
	return os.WriteFile(credentialsFilePath(), data, 0600)
}

func run() int {
	dbFlag := flag.String("db", "", "Path to cloudstorage.db for SQLite mode (prompted if omitted; mutually exclusive with -mysql)")
	mysqlFlag := flag.Bool("mysql", false, "Connect to MySQL/MariaDB instead of SQLite")
	hostFlag := flag.String("host", "", "MySQL host (prompted if omitted and not already saved)")
	portFlag := flag.Int("port", 0, "MySQL port (default 3306; prompted if omitted and not already saved)")
	userFlag := flag.String("user", "", "MySQL user (prompted if omitted and not already saved)")
	passwordFlag := flag.String("password", "", "MySQL password (prompted if omitted and not already saved)")
	databaseFlag := flag.String("database", "", "MySQL database name (prompted if omitted and not already saved)")
	noSaveFlag := flag.Bool("no-save", false, "Don't save connection details to remap-player-credentials.json")
	oldIDFlag := flag.String("old", "", "Old PlayerId to migrate FROM (prompted if omitted)")
	newIDFlag := flag.String("new", "", "New PlayerId to migrate TO (prompted if omitted)")
	flag.Parse()

	reader := bufio.NewReader(os.Stdin)

	saved, err := loadSavedCredentials()
	if err != nil {
		return fail("Could not read %s: %v\nDelete this file and run the tool again to reset it.",
			filepath.Base(credentialsFilePath()), err)
	}

	dbType, err := determineDBType(reader, *dbFlag, *mysqlFlag, saved)
	if err != nil {
		return fail("%v", err)
	}

	var db *sql.DB
	var backupPath string

	if dbType == "mysql" {
		creds := resolveMySQLCredentials(reader, *hostFlag, *portFlag, *userFlag, *passwordFlag, *databaseFlag, saved)

		dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", creds.User, creds.Password, creds.Host, creds.Port, creds.Database)
		db, err = sql.Open("mysql", dsn)
		if err != nil {
			return fail("Failed to open MySQL connection: %v", err)
		}
		defer db.Close()

		if err := db.Ping(); err != nil {
			return fail("Could not connect to MySQL at %s:%d as %s: %v", creds.Host, creds.Port, creds.User, err)
		}

		backupPath, err = backupMySQLDatabase(db, creds.Database)
		if err != nil {
			return fail("Failed to create a backup before proceeding: %v", err)
		}
		fmt.Printf("Backup created: %s\n", backupPath)

		if !*noSaveFlag && saved == nil {
			maybeSaveCredentials(reader, savedCredentials{DBType: "mysql", MySQL: &creds})
		}
	} else {
		dbPath := *dbFlag
		if dbPath == "" {
			dbPath = promptForDBPath(reader, saved)
		}

		absPath, absErr := filepath.Abs(dbPath)
		if absErr != nil {
			return fail("Could not resolve database path: %v", absErr)
		}
		dbPath = absPath

		if info, statErr := os.Stat(dbPath); statErr != nil {
			return fail("Database file not found at %s: %v", dbPath, statErr)
		} else if info.IsDir() {
			resolved, resolveErr := resolveDBPathFromFolder(dbPath, reader)
			if resolveErr != nil {
				return fail("%v", resolveErr)
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

		backupPath, err = backupDatabase(dbPath)
		if err != nil {
			return fail("Failed to create a backup before proceeding: %v", err)
		}
		fmt.Printf("Backup created: %s\n", backupPath)

		db, err = sql.Open("sqlite", dbPath)
		if err != nil {
			return fail("Failed to open database: %v", err)
		}
		defer db.Close()

		if !*noSaveFlag && saved == nil {
			maybeSaveCredentials(reader, savedCredentials{DBType: "sqlite", SQLitePath: dbPath})
		}
	}

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

	var merged, moved int
	if dbType == "mysql" {
		merged, moved, err = remapOwnerMySQL(db, oldID, newID)
	} else {
		merged, moved, err = remapOwnerSQLite(db, oldID, newID)
	}
	if err != nil {
		return fail("Migration failed: %v\nYour original data is safe in the backup: %s", err, backupPath)
	}

	fmt.Println()
	fmt.Printf("Done. %d row(s) merged into existing entries, %d row(s) moved directly.\n", merged, moved)
	fmt.Println("If anything looks wrong, restore from the backup file listed above.")
	return 0
}

// determineDBType figures out which backend to use for this run, in this
// order of precedence: an explicit -db (SQLite) or -mysql flag always
// wins; failing that, a previously saved credentials file's DBType is
// used automatically; failing that, the player is asked directly.
func determineDBType(reader *bufio.Reader, dbFlag string, mysqlFlag bool, saved *savedCredentials) (string, error) {
	if mysqlFlag && dbFlag != "" {
		return "", fmt.Errorf("-db and -mysql can't both be set — pick one backend")
	}
	if mysqlFlag {
		return "mysql", nil
	}
	if dbFlag != "" {
		return "sqlite", nil
	}
	if saved != nil && (saved.DBType == "sqlite" || saved.DBType == "mysql") {
		return saved.DBType, nil
	}

	for {
		fmt.Println("Which database is this cluster using?")
		fmt.Println("  [1] SQLite (solo-player Go-launcher)")
		fmt.Println("  [2] MySQL/MariaDB (cluster operator)")
		fmt.Print("Enter 1 or 2: ")
		line, _ := reader.ReadString('\n')
		switch strings.TrimSpace(line) {
		case "1":
			return "sqlite", nil
		case "2":
			return "mysql", nil
		default:
			fmt.Println("  Not a valid choice — try again.")
		}
	}
}

// maybeSaveCredentials offers to persist connection details after a
// successful connection, only when nothing was already saved (an
// existing saved file is never silently overwritten — deleting it is the
// deliberate reset action, per this tool's design).
func maybeSaveCredentials(reader *bufio.Reader, toSave savedCredentials) {
	fmt.Print("\nSave these connection details for next time? [Y/n]: ")
	line, _ := reader.ReadString('\n')
	if strings.EqualFold(strings.TrimSpace(line), "n") {
		return
	}
	if err := saveCredentials(toSave); err != nil {
		fmt.Printf("(Could not save credentials: %v — you'll be asked again next time.)\n", err)
		return
	}
	fmt.Printf("Saved to %s. Delete that file any time to be asked again (e.g. after a password change).\n",
		filepath.Base(credentialsFilePath()))
}

// resolveMySQLCredentials builds the connection details to use, in this
// order of precedence per field: an explicit flag always wins; failing
// that, a previously saved value; failing that, the player is prompted.
// This means a player can override just one saved field via a flag (e.g.
// a new password after a rotation) without retyping everything else.
func resolveMySQLCredentials(reader *bufio.Reader, hostFlag string, portFlag int, userFlag, passwordFlag, databaseFlag string, saved *savedCredentials) mysqlCredentials {
	var existing mysqlCredentials
	if saved != nil && saved.MySQL != nil {
		existing = *saved.MySQL
	}

	creds := mysqlCredentials{
		Host:     firstNonEmpty(hostFlag, existing.Host),
		Port:     firstNonZero(portFlag, existing.Port),
		User:     firstNonEmpty(userFlag, existing.User),
		Password: firstNonEmpty(passwordFlag, existing.Password),
		Database: firstNonEmpty(databaseFlag, existing.Database),
	}

	if creds.Host == "" {
		fmt.Print("MySQL host: ")
		line, _ := reader.ReadString('\n')
		creds.Host = strings.TrimSpace(line)
	}
	if creds.Port == 0 {
		fmt.Print("MySQL port [3306]: ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			creds.Port = 3306
		} else if p, err := strconv.Atoi(line); err == nil {
			creds.Port = p
		} else {
			creds.Port = 3306
		}
	}
	if creds.User == "" {
		fmt.Print("MySQL user: ")
		line, _ := reader.ReadString('\n')
		creds.User = strings.TrimSpace(line)
	}
	if creds.Password == "" {
		fmt.Print("MySQL password: ")
		line, _ := reader.ReadString('\n')
		creds.Password = strings.TrimSpace(line)
	}
	if creds.Database == "" {
		fmt.Print("MySQL database name: ")
		line, _ := reader.ReadString('\n')
		creds.Database = strings.TrimSpace(line)
	}

	return creds
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
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
	candidates := dbPathCandidates(folder)

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

// dbPathCandidates returns the known locations a cloudstorage.db file
// might live relative to dir, covering every deployment layout this
// project actually ships (a plain SQLite file next to dir, the
// Deployables/NodeJS-style data/ subfolder, and clouddbGoLauncher's own
// extracted app/data/ subfolder). Single source of truth for both
// findExistingDBCandidate's default-suggestion guess and
// resolveDBPathFromFolder's fallback search — these two used to carry
// separate, silently-diverged copies of this list, which is exactly how
// the launcher-relative path went unrecognized by the default guess
// despite already being known to the fallback search.
func dbPathCandidates(dir string) []string {
	return []string{
		filepath.Join(dir, "cloudstorage.db"),
		filepath.Join(dir, "data", "cloudstorage.db"),
		filepath.Join(dir, "app", "data", "cloudstorage.db"),
	}
}

// findExistingDBCandidate checks dbPathCandidates in order, returning the
// first one that actually exists on disk. Used by promptForDBPath to
// suggest a default that's actually correct for the layout it's running
// from, rather than a single hardcoded guess that only matches one of
// several real deployment shapes.
func findExistingDBCandidate(dir string) string {
	for _, c := range dbPathCandidates(dir) {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

func promptForDBPath(reader *bufio.Reader, saved *savedCredentials) string {
	defaultPath := ""
	if saved != nil && saved.SQLitePath != "" {
		defaultPath = saved.SQLitePath
	} else {
		exePath, err := os.Executable()
		exeDir := "."
		if err == nil {
			exeDir = filepath.Dir(exePath)
		}
		if found := findExistingDBCandidate(exeDir); found != "" {
			defaultPath = found
		} else {
			// No known layout matched — fall back to the original guess
			// rather than suggesting nothing, since this is still the
			// most likely path for a from-source dev-tree build.
			defaultPath = filepath.Join(exeDir, "data", "cloudstorage.db")
		}
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

// backupMySQLDatabase makes a best-effort logical backup of the entire
// DedicatedStorage table before any change is made — no flag to skip
// this, same "unconditional" principle as the SQLite path's file copy.
// Unlike SQLite, there's no single file to copy for a networked
// database, so this writes out every existing row as a plain SQL script
// (using INSERT ... ON DUPLICATE KEY UPDATE, so replaying it restores
// original amounts even if a row still exists) that can be run by hand
// via any MySQL client if a restore is ever needed.
//
// This is a safety net for manual recovery, not a one-click restore the
// way the SQLite backup file is — worth knowing before relying on it.
func backupMySQLDatabase(db *sql.DB, databaseName string) (string, error) {
	rows, err := db.Query(`SELECT clusterId, ownerId, resourceId, amount FROM DedicatedStorage`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	timestamp := time.Now().Format("2006-01-02T15-04-05")
	backupPath := fmt.Sprintf("%s.pre-remap.%s.sql", databaseName, timestamp)

	f, err := os.Create(backupPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	fmt.Fprintf(f, "-- DedicatedStorage backup taken %s before a remap-player run.\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(f, "-- To restore a row, run the matching statement below against the '%s' database.\n\n", databaseName)

	rowCount := 0
	for rows.Next() {
		var clusterID, ownerID, resourceID string
		var amount int64
		if err := rows.Scan(&clusterID, &ownerID, &resourceID, &amount); err != nil {
			return "", err
		}
		fmt.Fprintf(f,
			"INSERT INTO DedicatedStorage (clusterId, ownerId, resourceId, amount) VALUES (%s, %s, %s, %d) "+
				"ON DUPLICATE KEY UPDATE amount = VALUES(amount);\n",
			quoteSQL(clusterID), quoteSQL(ownerID), quoteSQL(resourceID), amount,
		)
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	fmt.Fprintf(f, "\n-- %d row(s) backed up.\n", rowCount)
	return backupPath, nil
}

// quoteSQL does minimal single-quote escaping for embedding string
// values into the backup script above. This tool only ever writes
// clusterId/ownerId/resourceId values that originated from this same
// database, so this is a safety net against stray quote characters, not
// a defense against untrusted input.
func quoteSQL(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
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

// remapOwnerSQLite moves every DedicatedStorage row from oldID to newID.
//
// Rows that collide on (clusterId, resourceId) under newID — the
// expected, common case for a player recovering access on a server they
// were already active on, not an edge case: clusterId is unchanged and
// resourceId is drawn from ARK's fixed set of resource types, so overlap
// between old and new rows is likely — have their amounts SUMMED into
// the existing newID row, and the oldID row is then deleted. Rows with
// no collision are moved directly via a plain UPDATE. The whole
// operation runs inside one transaction, so a failure partway through
// leaves the database in its original state rather than a
// half-migrated one.
//
// See remapOwnerMySQL for the MySQL/MariaDB equivalent — the two can't
// share one query set, because MySQL rejects a subquery that selects
// from the same table being updated/deleted in that statement (see that
// function's comment for the specific error and why a JOIN-based
// rewrite is needed there instead).
func remapOwnerSQLite(db *sql.DB, oldID, newID string) (merged int, moved int, err error) {
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

// remapOwnerMySQL performs the same logical migration as remapOwnerSQLite
// (see its comment for the full merge/move reasoning), but uses MySQL's
// multi-table UPDATE/DELETE ... JOIN syntax instead of the SQLite
// version's correlated subqueries.
//
// MySQL rejects a subquery that selects from the same table being
// updated or deleted in that same statement — error 1093, "You can't
// specify target table 'DedicatedStorage' for update in FROM clause."
// A JOIN-based multi-table statement sidesteps that restriction entirely
// (both table references are explicit in the UPDATE/DELETE's own JOIN
// clause rather than a nested subquery) and is the standard MySQL idiom
// for this kind of self-referencing update.
func remapOwnerMySQL(db *sql.DB, oldID, newID string) (merged int, moved int, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// Step 1: same merge as the SQLite version, expressed as a multi-table
	// UPDATE ... JOIN instead of a correlated subquery.
	mergeResult, err := tx.Exec(`
		UPDATE DedicatedStorage AS newRow
		JOIN DedicatedStorage AS oldRow
		  ON oldRow.clusterId = newRow.clusterId
		 AND oldRow.resourceId = newRow.resourceId
		 AND oldRow.ownerId = ?
		SET newRow.amount = newRow.amount + oldRow.amount
		WHERE newRow.ownerId = ?
	`, oldID, newID)
	if err != nil {
		return 0, 0, fmt.Errorf("merging colliding rows: %w", err)
	}
	mergedCount, _ := mergeResult.RowsAffected()

	// Step 2: remove the now-redundant oldID rows that were just merged
	// in, via a multi-table DELETE ... JOIN.
	if _, err = tx.Exec(`
		DELETE oldRow FROM DedicatedStorage AS oldRow
		JOIN DedicatedStorage AS newRow
		  ON newRow.ownerId = ?
		 AND newRow.clusterId = oldRow.clusterId
		 AND newRow.resourceId = oldRow.resourceId
		WHERE oldRow.ownerId = ?
	`, newID, oldID); err != nil {
		return 0, 0, fmt.Errorf("removing merged old rows: %w", err)
	}

	// Step 3: everything remaining under oldID had no collision — safe to
	// move directly. No subquery involved here, so this is identical to
	// the SQLite version.
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
