package state

type APIKeyUsageGroup struct {
	APIKeyName  string
	Model       string
	ServiceTier string
	Usage       Usage
	Responses   int64
}

func (s *Store) APIKeyUsageGroups() ([]APIKeyUsageGroup, error) {
	rows, err := s.db.Query(`SELECT api_key_name, model, service_tier, input_tokens, cached_tokens,
		cache_write_tokens, output_tokens, count(*) FROM response_usage
		WHERE api_key_name IS NOT NULL
		GROUP BY api_key_name, model, service_tier, input_tokens, cached_tokens, cache_write_tokens, output_tokens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []APIKeyUsageGroup
	for rows.Next() {
		var group APIKeyUsageGroup
		if err := rows.Scan(&group.APIKeyName, &group.Model, &group.ServiceTier, &group.Usage.InputTokens,
			&group.Usage.CachedTokens, &group.Usage.CacheWriteTokens, &group.Usage.OutputTokens, &group.Responses); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}
