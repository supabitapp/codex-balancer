package state

import "time"

func (s *Store) ThreadUsageEventsSince(kind string, start time.Time) ([]Event, error) {
	rows, err := s.db.Query(`SELECT at_ns, account_id, thread_key, model, service_tier, input_tokens, cached_tokens,
		cache_write_tokens, output_tokens, total_tokens, reasoning_tokens FROM events
		WHERE kind = ? AND at_ns >= ? ORDER BY id`, kind, start.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var at int64
		if err := rows.Scan(&at, &event.Account, &event.Thread, &event.Model, &event.ServiceTier, &event.Usage.InputTokens,
			&event.Usage.CachedTokens, &event.Usage.CacheWriteTokens, &event.Usage.OutputTokens,
			&event.Usage.TotalTokens, &event.Usage.ReasoningTokens); err != nil {
			return nil, err
		}
		event.At = time.Unix(0, at)
		events = append(events, event)
	}
	return events, rows.Err()
}
