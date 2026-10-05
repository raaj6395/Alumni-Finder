package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"
)

// Migrations run in order; PRAGMA user_version records how many have been applied.
// Append new entries, never edit old ones.
var migrations = []string{`
CREATE TABLE companies (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	normalized_name TEXT NOT NULL UNIQUE,
	slug TEXT NOT NULL DEFAULT '',
	slug_source TEXT NOT NULL DEFAULT '' CHECK (slug_source IN ('', 'xray', 'guess', 'manual')),
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE people (
	id INTEGER PRIMARY KEY,
	company_id INTEGER NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
	profile_url TEXT NOT NULL,
	profile_slug TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL DEFAULT '',
	headline TEXT NOT NULL DEFAULT '',
	raw_title TEXT NOT NULL DEFAULT '',
	snippet TEXT NOT NULL DEFAULT '',
	score INTEGER NOT NULL DEFAULT 0,
	confidence TEXT NOT NULL DEFAULT '',
	score_reasons TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL DEFAULT 'new'
		CHECK (status IN ('new','request_sent','accepted','messaged','replied','referred','not_relevant','no_response')),
	notes TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX people_company ON people(company_id);
CREATE TABLE search_cache (
	key TEXT PRIMARY KEY,
	provider TEXT NOT NULL,
	query TEXT NOT NULL,
	page INTEGER NOT NULL,
	response_json TEXT NOT NULL,
	fetched_at TEXT NOT NULL
);
CREATE TABLE api_usage (
	date TEXT NOT NULL,
	provider TEXT NOT NULL,
	count INTEGER NOT NULL,
	PRIMARY KEY (date, provider)
);
`}

func openDB(path string) (*sql.DB, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single user; avoids SQLITE_BUSY entirely
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return nil, err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := tx.Exec(`PRAGMA user_version = ` + strconv.Itoa(i+1)); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return db, nil
}
