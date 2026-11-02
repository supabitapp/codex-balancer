package state

import "time"

type UsageRow struct {
	ID          int64
	APIKeyName  string
	At          time.Time
	Model       string
	ServiceTier string
	Usage       Usage
}

func (s *Store) UsageRowsAfter(id int64, limit int) ([]UsageRow, error) {
	rows, err := s.db.Query(`SELECT id, coalesce(api_key_name, ''), at_ns, model, service_tier, input_tokens, cached_tokens,
		cache_write_tokens, output_tokens, reasoning_tokens FROM response_usage
		WHERE id > ? ORDER BY id LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []UsageRow
	for rows.Next() {
		var record UsageRow
		var at int64
		if err := rows.Scan(&record.ID, &record.APIKeyName, &at, &record.Model, &record.ServiceTier,
			&record.Usage.InputTokens, &record.Usage.CachedTokens, &record.Usage.CacheWriteTokens,
			&record.Usage.OutputTokens, &record.Usage.ReasoningTokens); err != nil {
			return nil, err
		}
		record.At = decodeTime(at)
		records = append(records, record)
	}
	return records, rows.Err()
}
