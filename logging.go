package main

import (
	"log/slog"
	"time"
)

func (s *server) pickAccount(thread, pinned string, skip map[string]bool, attempt int, via transport) *Account {
	decision := s.pool.route(pinned, skip)
	s.log.Debug("routing attempt",
		"transport", via,
		"thread", thread,
		"attempt", attempt+1,
		"pinned_account", pinned,
		"accounts", len(decision.candidates),
	)
	for _, candidate := range decision.candidates {
		attrs := []any{
			"transport", via,
			"thread", thread,
			"attempt", attempt + 1,
			"selected", candidate.account == decision.account,
			"pinned", candidate.id == pinned,
			"skipped", skip[candidate.id],
		}
		attrs = append(attrs, routingLogAttrs(candidate, decision.now)...)
		s.log.Debug("routing candidate", attrs...)
	}
	if decision.account == nil {
		s.log.Warn("no account available",
			"transport", via,
			"thread", thread,
			"attempt", attempt+1,
			"pinned_account", pinned,
		)
	}
	return decision.account
}

func routingLogAttrs(candidate routingCandidate, now time.Time) []any {
	return []any{
		"account", candidate.id,
		"status", candidate.status(now),
		"cooldown_until", candidate.cooldown,
		"last_used_at", candidate.lastUsed,
		"reauth", candidate.reauth,
		"primary", windowLogValue(candidate.primary),
		"secondary", windowLogValue(candidate.secondary),
	}
}

func windowLogValue(value window) slog.Value {
	return slog.GroupValue(
		slog.Bool("known", value.known()),
		slog.Float64("used_percent", value.usedPercent),
		slog.Int("window_minutes", value.minutes),
		slog.Time("reset_at", value.resetsAt),
		slog.Time("seen_at", value.seenAt),
	)
}
