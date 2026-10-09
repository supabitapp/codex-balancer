package app

import "time"

func (s *StateStore) apiKeyCosts(prices priceSnapshot, start time.Time) (map[string]usageCost, error) {
	groups, err := s.raw.APIKeyUsageGroups(start)
	if err != nil {
		return nil, err
	}
	costs := make(map[string]usageCost)
	for _, group := range groups {
		cost := costs[group.APIKeyName]
		price, known := prices.estimate(group.Model, group.ServiceTier, responseUsageFromState(group.Usage))
		if known {
			cost.apiCostNanoDollars += price * group.Responses
		} else {
			cost.unpricedResponses += group.Responses
		}
		costs[group.APIKeyName] = cost
	}
	return costs, nil
}
