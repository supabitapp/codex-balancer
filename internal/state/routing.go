package state

import "fmt"

func (s *Store) migrateRoutingModes() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Replace only the constrained column. Rebuilding accounts would risk
	// firing response_usage's ON DELETE SET NULL foreign-key action.
	if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN next_routing_mode TEXT NOT NULL DEFAULT 'normal'
		CHECK (next_routing_mode IN ('normal', 'priority'));
		UPDATE accounts SET next_routing_mode = CASE routing_mode WHEN 'priority' THEN 'priority' ELSE 'normal' END;
		ALTER TABLE accounts DROP COLUMN routing_mode;
		ALTER TABLE accounts RENAME COLUMN next_routing_mode TO routing_mode;`); err != nil {
		return fmt.Errorf("migrate routing modes: %w", err)
	}
	if _, err := tx.Exec("PRAGMA user_version = 7"); err != nil {
		return err
	}
	return tx.Commit()
}
