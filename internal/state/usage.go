package state

import "time"

type APIKeyUsageRow struct {
	ID          int64
	APIKeyName  string
	At          time.Time
	Model       string
	ServiceTier string
	Usage       Usage
}

func (s *Store) APIKeyUsageAfter(id int64, limit int) ([]APIKeyUsageRow, error) {
	rows, err := s.db.Query(`SELECT id, api_key_name, at_ns, model, service_tier, input_tokens, cached_tokens,
		cache_write_tokens, output_tokens, reasoning_tokens FROM response_usage
		WHERE id > ? AND api_key_name IS NOT NULL ORDER BY id LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []APIKeyUsageRow
	for rows.Next() {
		var record APIKeyUsageRow
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
