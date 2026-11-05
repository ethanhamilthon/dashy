package agent

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

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

var migrations = []string{
	`ALTER TABLE sessions ADD COLUMN provider TEXT NOT NULL DEFAULT '';
	 ALTER TABLE sessions ADD COLUMN model TEXT NOT NULL DEFAULT '';
	 ALTER TABLE sessions ADD COLUMN effort TEXT NOT NULL DEFAULT '';`,
	`CREATE TABLE tasks (
		session   TEXT NOT NULL,
		id        TEXT NOT NULL,
		command   TEXT NOT NULL,
		status    TEXT NOT NULL,
		exit_code INTEGER,
		started   INTEGER NOT NULL,
		ended     INTEGER,
		log       TEXT NOT NULL,
		PRIMARY KEY (session, id)
	);`,
	`ALTER TABLE items ADD COLUMN meta TEXT NOT NULL DEFAULT '';`,
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= len(migrations) {
		return nil
	}
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	for _, m := range migrations[version:] {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}

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
		err = migrate(db)
	}
	if err != nil {
		if db != nil {
			db.Close()
		}
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	return db, nil
}
