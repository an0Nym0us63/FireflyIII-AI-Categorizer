// Package aidb is the classifier's own SQLite store mapping a Firefly
// transaction ID to the AI metadata (outcome, confidence, pending tag
// suggestions, review state). Keeping this out of Firefly means no control
// tags pollute the user's Firefly tag statistics.
package aidb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Record is the AI metadata for one transaction.
type Record struct {
	TransactionID  string
	Outcome        string // CLASSIFIED | ASSUMED | NEEDS_REVIEW
	Category       string // category the AI chose (for reference)
	DestConfidence string // "" | CLASSIFIED | ASSUMED
	Reason         string
	Assumption     string
	SuggestedTags  []string // pending semantic tags awaiting validation
	Reviewed       bool
	UpdatedAt      time.Time
}

// DB wraps the SQLite connection.
type DB struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and migrates it.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open ai db: %w", err)
	}
	// SQLite handles one writer at a time; serialise to avoid "database is locked".
	db.SetMaxOpenConns(1)

	const schema = `
CREATE TABLE IF NOT EXISTS ai_records (
	transaction_id  TEXT PRIMARY KEY,
	outcome         TEXT,
	category        TEXT,
	dest_confidence TEXT,
	reason          TEXT,
	assumption      TEXT,
	suggested_tags  TEXT,
	reviewed        INTEGER DEFAULT 0,
	updated_at      TEXT
);
CREATE TABLE IF NOT EXISTS jobs (
	id         TEXT PRIMARY KEY,
	data       TEXT NOT NULL,
	created_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate ai db: %w", err)
	}
	return &DB{db: db}, nil
}

func (d *DB) Close() error { return d.db.Close() }

// Upsert inserts or replaces a record.
func (d *DB) Upsert(r Record) error {
	tags, _ := json.Marshal(r.SuggestedTags)
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = time.Now()
	}
	_, err := d.db.Exec(`
INSERT INTO ai_records
	(transaction_id, outcome, category, dest_confidence, reason, assumption, suggested_tags, reviewed, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(transaction_id) DO UPDATE SET
	outcome=excluded.outcome, category=excluded.category, dest_confidence=excluded.dest_confidence,
	reason=excluded.reason, assumption=excluded.assumption, suggested_tags=excluded.suggested_tags,
	reviewed=excluded.reviewed, updated_at=excluded.updated_at`,
		r.TransactionID, r.Outcome, r.Category, r.DestConfidence, r.Reason, r.Assumption,
		string(tags), boolToInt(r.Reviewed), r.UpdatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("upsert ai record: %w", err)
	}
	return nil
}

// Get returns the record for id, or (nil, nil) when absent.
func (d *DB) Get(id string) (*Record, error) {
	row := d.db.QueryRow(`SELECT transaction_id, outcome, category, dest_confidence, reason, assumption, suggested_tags, reviewed, updated_at FROM ai_records WHERE transaction_id = ?`, id)
	rec, err := scanRecord(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// GetMany returns records for the given ids, keyed by transaction ID.
func (d *DB) GetMany(ids []string) (map[string]Record, error) {
	out := make(map[string]Record)
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := d.db.Query(`SELECT transaction_id, outcome, category, dest_confidence, reason, assumption, suggested_tags, reviewed, updated_at FROM ai_records WHERE transaction_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out[rec.TransactionID] = *rec
	}
	return out, rows.Err()
}

// PendingReview returns records that still need human attention: not yet
// reviewed and either assumed/needs-review, an assumed destination, or with
// pending tag suggestions.
func (d *DB) PendingReview() ([]Record, error) {
	rows, err := d.db.Query(`
SELECT transaction_id, outcome, category, dest_confidence, reason, assumption, suggested_tags, reviewed, updated_at
FROM ai_records
WHERE reviewed = 0 AND (
	outcome IN ('ASSUMED','NEEDS_REVIEW')
	OR dest_confidence = 'ASSUMED'
	OR (suggested_tags IS NOT NULL AND suggested_tags NOT IN ('', '[]', 'null'))
)
ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// MarkTreated flags a transaction as treated/reviewed, creating a record if
// none exists (used for the manual "mark as treated" bulk action).
func (d *DB) MarkTreated(id string) error {
	_, err := d.db.Exec(`
INSERT INTO ai_records (transaction_id, outcome, reviewed, suggested_tags, updated_at)
VALUES (?, 'REVIEWED', 1, '[]', ?)
ON CONFLICT(transaction_id) DO UPDATE SET reviewed = 1, updated_at = excluded.updated_at`,
		id, time.Now().Unix())
	return err
}

// MarkReviewed flags a transaction as human-reviewed and clears its pending tags.
// ListReviewed returns the most recently human-reviewed records.
func (d *DB) ListReviewed(limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.db.Query(`
SELECT transaction_id, outcome, category, dest_confidence, reason, assumption, suggested_tags, reviewed, updated_at
FROM ai_records WHERE reviewed = 1 ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// Unreview clears the reviewed flag so a transaction returns to the review list.
func (d *DB) Unreview(id string) error {
	_, err := d.db.Exec(`UPDATE ai_records SET reviewed = 0, updated_at = ? WHERE transaction_id = ?`,
		time.Now().Unix(), id)
	return err
}

func (d *DB) MarkReviewed(id string) error {
	_, err := d.db.Exec(`UPDATE ai_records SET reviewed = 1, suggested_tags = '[]', updated_at = ? WHERE transaction_id = ?`,
		time.Now().Unix(), id)
	return err
}

// SetSuggestedTags replaces the pending tag suggestions for a transaction.
func (d *DB) SetSuggestedTags(id string, tags []string) error {
	b, _ := json.Marshal(tags)
	_, err := d.db.Exec(`UPDATE ai_records SET suggested_tags = ?, updated_at = ? WHERE transaction_id = ?`,
		string(b), time.Now().Unix(), id)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRecord(s scanner) (*Record, error) {
	var r Record
	var tagsJSON string
	var reviewed int
	var updated string
	if err := s.Scan(&r.TransactionID, &r.Outcome, &r.Category, &r.DestConfidence, &r.Reason, &r.Assumption, &tagsJSON, &reviewed, &updated); err != nil {
		return nil, err
	}
	if tagsJSON != "" {
		_ = json.Unmarshal([]byte(tagsJSON), &r.SuggestedTags)
	}
	r.Reviewed = reviewed != 0
	if t, err := time.Parse(time.RFC3339, updated); err == nil {
		r.UpdatedAt = t
	}
	return &r, nil
}

// SaveJobJSON upserts a job's JSON blob, preserving its original created_at.
func (d *DB) SaveJobJSON(id, data string) error {
	_, err := d.db.Exec(`
INSERT INTO jobs (id, data, created_at) VALUES (?, ?, ?)
ON CONFLICT(id) DO UPDATE SET data=excluded.data`, id, data, time.Now().Unix())
	return err
}

// DeleteAllJobs removes every persisted job (clears the Jobs log).
func (d *DB) DeleteAllJobs() error {
	_, err := d.db.Exec(`DELETE FROM jobs`)
	return err
}

// LoadJobsJSON returns the most recent job JSON blobs (newest first).
func (d *DB) LoadJobsJSON(limit int) ([]string, error) {
	if limit <= 0 {
		limit = 2000
	}
	rows, err := d.db.Query(`SELECT data FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		out = append(out, data)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
