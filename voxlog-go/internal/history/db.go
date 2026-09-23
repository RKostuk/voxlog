package history

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// The database is where a meeting's structure lives: its turns, who spoke in
// each one, and the voice embeddings that tell one speaker from another
// across meetings. None of that fits the one-JSON-file-per-meeting shape the
// store started with -- "how long did each person talk" is an aggregate, and
// "which unnamed voice is closest to this one" is a scan, and both would mean
// reading every file on disk to answer one question.
//
// Audio deliberately stays out. Recordings remain plain WAV files beside the
// transcripts: playing one reply is a seek, and http.ServeContent (see
// internal/ui/server.go) already streams byte ranges straight from the file.
// A blob would have to be materialised whole -- about 115 MB per hour of call
// -- for every click, retention would become DELETE plus VACUUM instead of
// os.Remove, and every backup would copy a database that grows by gigabytes a
// week. Only small things go in: turn text and timings, embeddings, voice
// names, and one short sample clip per named voice.
//
// The pure-Go driver (modernc.org/sqlite) is chosen over the cgo one on
// purpose. cgo is already in the build for the audio and ASR libraries, but
// those are prebuilt archives; mattn/go-sqlite3 would add a real C compile to
// every build. More to the point, this app already has to survive crashes
// inside cgo (see the panic recovery in decodequeue.go) -- the storage layer
// should not be able to take the process down with it.
type DB struct {
	sql *sql.DB
}

// OpenDB opens (creating if needed) the database at path.
//
// SetMaxOpenConns(1) is not a throughput compromise, it is the concurrency
// design: one connection serialises every writer, which is what the stores'
// mutexes used to do, and this app writes a few rows per meeting -- there is
// nothing to gain from parallel writers and a torn read-modify-write to lose.
func OpenDB(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("history: creating database directory: %w", err)
	}

	// WAL so a read (the window listing meetings) never blocks a write (a
	// transcript landing); synchronous=NORMAL because with WAL that only
	// risks the last commits on a power cut, and a lost transcript is
	// recoverable from audio while a stalled UI is not; busy_timeout so a
	// concurrent writer waits rather than failing outright.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_txlock=immediate"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: opening database %s: %w", path, err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("history: opening database %s: %w", path, err)
	}

	db := &DB{sql: sqlDB}
	if err := db.migrateSchema(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

// SQL hands out the connection behind this database.
//
// It exists for internal/task, which keeps its own tables in this same file:
// there is one database and one ordered list of schema steps (see schema.go),
// so a second package with a second database would mean a second migration
// history, two files to back up, and no way to ever join a task to the
// meeting it came out of. The alternative -- moving task storage in here --
// would mean this package importing internal/task, which imports it back.
func (d *DB) SQL() *sql.DB { return d.sql }
