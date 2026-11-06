package state

const accountCreditsSchema = `
ALTER TABLE accounts ADD COLUMN allow_credits INTEGER NOT NULL DEFAULT 0 CHECK (allow_credits IN (0, 1));`

func (s *Store) migrateAccountCredits() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var present int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('accounts') WHERE name = 'allow_credits'").Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		if _, err := tx.Exec(accountCreditsSchema); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("PRAGMA user_version = 10"); err != nil {
		return err
	}
	return tx.Commit()
}
