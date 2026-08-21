package main

import (
	"context"
	"database/sql"
	"os"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/rotisserie/eris"
	_ "modernc.org/sqlite"

	"twitter-bookmarks-downloader/ent"
)

var DB *ent.Client

// InitDB decides what kind of file it is looking at before any pool or ent
// machinery touches it: a fresh path (missing or zero-length file) first gets
// its creation-time header properties stamped; an existing file must prove it
// is not a GORM-era database. Only then is the pool opened and ent allowed to
// create or verify the schema, which is idempotent on an up-to-date file.
func InitDB(dbPath string) error {
	fresh, err := isFreshDatabase(dbPath)
	if err != nil {
		return err
	}
	if fresh {
		if err := stampCreationProperties(dbPath); err != nil {
			return err
		}
	} else if err := ensureNotGormEra(dbPath); err != nil {
		return err
	}

	// modernc.org/sqlite applies _pragma parameters on every new connection,
	// which is what per-connection settings need under database/sql pooling.
	pragmas := []string{
		"foreign_keys(1)",
		"busy_timeout(5000)",
		"journal_mode(WAL)",
		// The documented safe pairing with WAL: a power cut can lose the tail
		// of the log but cannot corrupt the file.
		"synchronous(NORMAL)",
		"cache_size(-20000)", // negative means KiB: 20 MB
		"temp_store(MEMORY)",
		// Verified to actually take effect under modernc's pure-Go
		// implementation.
		"mmap_size(2147483648)",
	}
	dsn := "file:" + dbPath + "?_pragma=" + strings.Join(pragmas, "&_pragma=")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return eris.Wrap(err, "failed to open database")
	}

	DB = ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))

	// A fresh or empty database gets the full schema here; an up-to-date
	// database is a no-op. Versioned migrations are deferred until the first
	// schema change after release.
	if err := DB.Schema.Create(context.Background()); err != nil {
		return eris.Wrap(err, "failed to create schema")
	}
	return nil
}

// isFreshDatabase reports whether dbPath needs to be created from scratch. A
// zero-length file counts as fresh: SQLite leaves one behind when a connection
// opens a path but never writes.
func isFreshDatabase(dbPath string) (bool, error) {
	info, err := os.Stat(dbPath)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, eris.Wrap(err, "failed to stat database file")
	}
	return info.Size() == 0, nil
}

// stampCreationProperties fixes the file-level properties that SQLite freezes
// into the header on first write and silently ignores ever after: page size
// and auto-vacuum mode. They cannot ride the pool's _pragma DSN: the other
// pragmas there (journal_mode among them) initialize the file the moment a
// connection opens, and once the header is written and the file is in WAL
// mode, SQLite discards any later page_size. Observed directly: combining
// page_size with any second DSN pragma leaves the default 4096 in place. So a
// bare bootstrap connection sets both properties and VACUUM forces the header
// out before the pool or ent ever touch the file.
func stampCreationProperties(dbPath string) error {
	boot, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		return eris.Wrap(err, "failed to open database for creation")
	}
	defer CloseResource(boot)

	for _, pragma := range []string{
		"PRAGMA page_size = 8192",
		"PRAGMA auto_vacuum = INCREMENTAL",
		"VACUUM",
	} {
		if _, err := boot.Exec(pragma); err != nil {
			return eris.Wrapf(err, "failed to initialize database (%s)", pragma)
		}
	}
	return nil
}

// ensureNotGormEra refuses to run against a database created by the GORM-era
// releases. Letting ent's migration touch it would silently graft new columns
// onto the dirty old schema instead of rebuilding records from raw_json, so
// the only safe move is to stop and point the user at the migration command.
// InitDB calls this before ent.NewClient and Schema.Create exist, so ent never
// sees an old file at all.
//
// The fingerprint is the rating column: it exists only in the ent-era field
// model, and no GORM-era variant of the tweets table has it, neither the
// clean shape nor the ones carrying the stray legacy/bookmarked columns that
// AutoMigrate left behind. One column name separates the generations without
// a version table or a schema hash.
//
// The probe opens its own read-only connection: the pool's DSN carries
// journal_mode(WAL), and merely inspecting an old file through it would
// persistently switch that file to WAL and drop -wal/-shm files next to it,
// which contradicts the promise that migration leaves the old database
// untouched.
func ensureNotGormEra(dbPath string) error {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return eris.Wrap(err, "failed to open database for inspection")
	}
	defer CloseResource(db)

	var tables int
	err = db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tweets'`,
	).Scan(&tables)
	if err != nil {
		return eris.Wrap(err, "failed to inspect database schema")
	}
	if tables == 0 {
		return nil
	}

	rows, err := db.Query(`PRAGMA table_info(tweets)`)
	if err != nil {
		return eris.Wrap(err, "failed to read tweets table info")
	}
	defer func() {
		if err := rows.Close(); err != nil {
			PrintWarningF("Failed to close table info rows: %v", err)
		}
	}()

	for rows.Next() {
		var (
			cid, notNull, pk int
			name, colType    string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return eris.Wrap(err, "failed to scan tweets table info")
		}
		if name == "rating" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return eris.Wrap(err, "failed to read tweets table info")
	}

	return eris.New("this database was created by an older, GORM-era release; " +
		"run the forthcoming migration command (tbd -migrate) to convert it, " +
		"the old file will be left untouched")
}

func CloseDB() {
	if DB == nil {
		return
	}
	if err := DB.Close(); err != nil {
		PrintWarningF("Failed to close database: %v", err)
	}
}

// WithTx runs fn inside a transaction, committing on success and rolling back
// on error or panic.
func WithTx(ctx context.Context, fn func(tx *ent.Tx) error) error {
	tx, err := DB.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if v := recover(); v != nil {
			_ = tx.Rollback()
			panic(v)
		}
	}()
	if err := fn(tx); err != nil {
		if rerr := tx.Rollback(); rerr != nil {
			err = eris.Wrapf(err, "rolling back transaction: %v", rerr)
		}
		return err
	}
	return tx.Commit()
}
