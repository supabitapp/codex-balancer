package state

const adminSchema = `ALTER TABLE settings ADD COLUMN admin_password_hash TEXT NOT NULL DEFAULT '';`

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

func (s *Store) AdminPasswordHash() (string, error) {
	var hash string
	err := s.db.QueryRow("SELECT admin_password_hash FROM settings WHERE id = 1").Scan(&hash)
	return hash, err
}

func (s *Store) SetAdminPasswordHash(hash string) error {
	_, err := s.db.Exec("UPDATE settings SET admin_password_hash = ? WHERE id = 1", hash)
	return err
}
