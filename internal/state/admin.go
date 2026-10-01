package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const adminSchema = `ALTER TABLE settings ADD COLUMN admin_password_hash TEXT NOT NULL DEFAULT '';`

const adminSessionsSchema = `
CREATE TABLE IF NOT EXISTS admin_sessions (
	id BLOB PRIMARY KEY CHECK (length(id) = 32),
	csrf TEXT NOT NULL CHECK (length(csrf) > 0),
	credential BLOB NOT NULL CHECK (length(credential) = 32),
	expires_at_ns INTEGER NOT NULL CHECK (expires_at_ns > 0)
) STRICT, WITHOUT ROWID;`

type AdminSession struct {
	ID         []byte
	CSRF       string
	Credential []byte
	ExpiresAt  time.Time
}

func (s *Store) migrateAdmin() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(adminSchema); err != nil {
		return err
	}
	if _, err := tx.Exec("PRAGMA user_version = 5"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) migrateAdminSessions() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(adminSessionsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec("PRAGMA user_version = 9"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AdminPasswordHash() (string, error) {
	var hash string
	err := s.db.QueryRow("SELECT admin_password_hash FROM settings WHERE id = 1").Scan(&hash)
	return hash, err
}

func (s *Store) SetAdminPasswordHash(hash string) error {
	return s.immediate(func(conn *sql.Conn) error {
		ctx := context.Background()
		if _, err := conn.ExecContext(ctx, "UPDATE settings SET admin_password_hash = ? WHERE id = 1", hash); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "DELETE FROM admin_sessions")
		return err
	})
}

func (s *Store) CreateAdminSession(session AdminSession, now time.Time, limit int) error {
	return s.immediate(func(conn *sql.Conn) error {
		ctx := context.Background()
		if _, err := conn.ExecContext(ctx, "DELETE FROM admin_sessions WHERE expires_at_ns <= ?", encodeTime(now)); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM admin_sessions WHERE id IN (
			SELECT id FROM admin_sessions ORDER BY expires_at_ns DESC LIMIT -1 OFFSET ?)`, limit-1); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "INSERT INTO admin_sessions (id, csrf, credential, expires_at_ns) VALUES (?, ?, ?, ?)",
			session.ID, session.CSRF, session.Credential, encodeTime(session.ExpiresAt))
		return err
	})
}

func (s *Store) AdminSession(id []byte) (AdminSession, bool, error) {
	session := AdminSession{ID: id}
	var expiresAt int64
	err := s.db.QueryRow("SELECT csrf, credential, expires_at_ns FROM admin_sessions WHERE id = ?", id).
		Scan(&session.CSRF, &session.Credential, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminSession{}, false, nil
	}
	if err != nil {
		return AdminSession{}, false, err
	}
	session.ExpiresAt = decodeTime(expiresAt)
	return session, true, nil
}

func (s *Store) ExtendAdminSession(id []byte, expiresAt time.Time) error {
	_, err := s.db.Exec("UPDATE admin_sessions SET expires_at_ns = ? WHERE id = ?", encodeTime(expiresAt), id)
	return err
}

func (s *Store) DeleteAdminSession(id []byte) error {
	_, err := s.db.Exec("DELETE FROM admin_sessions WHERE id = ?", id)
	return err
}
