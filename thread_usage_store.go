package main

import "time"

func (s *StateStore) threadUsageEventsSince(start time.Time) ([]storedEvent, error) {
	rows, err := s.db.Query(`SELECT at_ns, account_id, thread_key, model, service_tier, input_tokens, cached_tokens,
		cache_write_tokens, output_tokens, total_tokens, reasoning_tokens FROM events
		WHERE kind = ? AND at_ns >= ? ORDER BY id`, eventResponseUsage, start.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []storedEvent
	for rows.Next() {
		var event storedEvent
		var at int64
		if err := rows.Scan(&at, &event.Account, &event.Thread, &event.Model, &event.ServiceTier, &event.Usage.InputTokens,
			&event.Usage.InputDetails.CachedTokens, &event.Usage.InputDetails.CacheWriteTokens, &event.Usage.OutputTokens,
			&event.Usage.TotalTokens, &event.Usage.OutputDetails.ReasoningTokens); err != nil {
			return nil, err
		}
		event.At = time.Unix(0, at)
		events = append(events, event)
	}
	return events, rows.Err()
}
