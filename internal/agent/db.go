package agent

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// The database holds settings (one JSON row with a version that grows on every
// save) and sessions with their history items, one row per item.
const schema = `
CREATE TABLE IF NOT EXISTS settings (
	id      INTEGER PRIMARY KEY CHECK (id = 1),
	data    TEXT NOT NULL,
	version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	id      TEXT PRIMARY KEY,
	project TEXT NOT NULL,
	title   TEXT NOT NULL DEFAULT '',
	first   TEXT NOT NULL DEFAULT '',
	updated INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_by_project ON sessions (project, updated);
CREATE TABLE IF NOT EXISTS items (
	session TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
	seq     INTEGER NOT NULL,
	data    TEXT NOT NULL,
	PRIMARY KEY (session, seq)
) WITHOUT ROWID;`

// DBPath is the database in DASHY_HOME.
func DBPath(home string) string { return filepath.Join(home, "dashy.db") }

// openDB opens the database in home, creating it private to the user. Several dashy
// processes share it: WAL lets readers run beside a writer, and writers wait for each other.
func openDB(home string) (*sql.DB, error) {
	if err := os.MkdirAll(home, 0700); err != nil {
		return nil, err
	}
	path := DBPath(home)
	// SQLite gives its -wal and -shm files the mode of the database file.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0600)
	if err != nil {
		return nil, err
	}
	file.Close()
	query := "_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: query}).String())
	if err == nil {
		_, err = db.Exec(schema)
	}
	if err != nil {
		if db != nil {
			db.Close()
		}
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	return db, nil
}
