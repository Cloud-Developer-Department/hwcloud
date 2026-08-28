// Package sqlite implements session.Store on top of SQLite.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// Store persists session metadata in a SQLite database.
type Store struct {
	db *sql.DB
}

// New creates a Store backed by the given *sql.DB.
func New(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("sqlite sessionstore: migrate: %w", err)
	}
	return s, nil
}

// DB returns the underlying *sql.DB for sharing with Memory backends.
func (s *Store) DB() *sql.DB { return s.db }

// ── session.Store ──

func (s *Store) Save(ctx context.Context, info session.SessionInfo) error {
	dirsJSON, _ := json.Marshal(info.AdditionalDirectories)
	metaJSON, _ := json.Marshal(info.Meta)
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO sessions
		 (id, cwd, title, created_at, updated_at, additional_directories, meta)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		info.ID, info.Cwd, info.Title,
		info.CreatedAt.Format(time.RFC3339), info.UpdatedAt.Format(time.RFC3339),
		string(dirsJSON), string(metaJSON),
	)
	return err
}

func (s *Store) Get(ctx context.Context, id string) (*session.SessionInfo, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, cwd, title, created_at, updated_at, additional_directories, meta
		 FROM sessions WHERE id = ?`, id)
	return scanInfo(row)
}

func (s *Store) List(ctx context.Context) ([]session.SessionInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, cwd, title, created_at, updated_at, additional_directories, meta
		 FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []session.SessionInfo
	for rows.Next() {
		info, err := scanRows(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *info)
	}
	if list == nil {
		list = []session.SessionInfo{}
	}
	return list, rows.Err()
}

func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// Close releases the underlying database connection.
func (s *Store) Close() error { return s.db.Close() }

// ── migration ──

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id                      TEXT PRIMARY KEY,
			cwd                     TEXT NOT NULL DEFAULT '',
			title                   TEXT NOT NULL DEFAULT '',
			created_at              TEXT NOT NULL DEFAULT '',
			updated_at              TEXT NOT NULL DEFAULT '',
			additional_directories  TEXT NOT NULL DEFAULT '[]',
			meta                    TEXT NOT NULL DEFAULT '{}'
		);
	`)
	return err
}

// ── helpers ──

type rowScanner interface {
	Scan(dest ...any) error
}

func scanInfo(row rowScanner) (*session.SessionInfo, error) {
	var (
		id, cwd, title, createdRaw, updatedRaw string
		dirsJSON, metaJSON                     string
	)
	if err := row.Scan(&id, &cwd, &title, &createdRaw, &updatedRaw, &dirsJSON, &metaJSON); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return buildInfo(id, cwd, title, createdRaw, updatedRaw, dirsJSON, metaJSON), nil
}

func scanRows(rows *sql.Rows) (*session.SessionInfo, error) {
	var (
		id, cwd, title, createdRaw, updatedRaw string
		dirsJSON, metaJSON                     string
	)
	if err := rows.Scan(&id, &cwd, &title, &createdRaw, &updatedRaw, &dirsJSON, &metaJSON); err != nil {
		return nil, err
	}
	return buildInfo(id, cwd, title, createdRaw, updatedRaw, dirsJSON, metaJSON), nil
}

func buildInfo(id, cwd, title, createdRaw, updatedRaw, dirsJSON, metaJSON string) *session.SessionInfo {
	created, _ := time.Parse(time.RFC3339, createdRaw)
	updated, _ := time.Parse(time.RFC3339, updatedRaw)

	var dirs []string
	if dirsJSON != "" && dirsJSON != "[]" {
		json.Unmarshal([]byte(dirsJSON), &dirs)
	}

	var meta map[string]any
	if metaJSON != "" && metaJSON != "{}" {
		json.Unmarshal([]byte(metaJSON), &meta)
	}

	return &session.SessionInfo{
		ID:                    id,
		Cwd:                   cwd,
		Title:                 title,
		CreatedAt:             created,
		UpdatedAt:             updated,
		AdditionalDirectories: dirs,
		Meta:                  meta,
	}
}
