package main

import (
	"log/slog"
	"time"
)

func (s *server) pickAccount(thread string, skip map[string]bool, attempt int) *Account {
	decision := s.pool.route(skip)
	s.log.Debug("routing attempt",
		"thread", thread,
		"attempt", attempt+1,
		"accounts", len(decision.candidates),
	)
	for _, candidate := range decision.candidates {
		attrs := []any{
			"thread", thread,
			"attempt", attempt + 1,
			"selected", candidate.account == decision.account,
			"skipped", skip[candidate.id],
		}
		attrs = append(attrs, routingLogAttrs(candidate, decision.now)...)
		s.log.Debug("routing candidate", attrs...)
	}
	if decision.account == nil {
		s.log.Warn("no account available",
			"thread", thread,
			"attempt", attempt+1,
		)
	}
	return decision.account
}

func routingLogAttrs(candidate routingCandidate, now time.Time) []any {
	status := candidate.status(now)
	priority, prioritized := candidate.routingPriority(now)
	prioritized = prioritized && status == accountPriority
	attrs := []any{
		"account", candidate.id,
		"status", status,
		"routing_mode", candidate.mode,
		"reset_priority", prioritized,
		"banked_resets_known", candidate.resetCredits.known,
		"banked_resets", candidate.resetCredits.count,
		"usage_limit_reached", candidate.spent,
		"cooldown_until", candidate.cooldown,
		"last_used_at", candidate.lastUsed,
		"reauth", candidate.reauth,
		"primary", windowLogValue(candidate.primary),
		"secondary", windowLogValue(candidate.secondary),
	}
	if prioritized {
		attrs = append(attrs,
			"reset_priority_expires_at", priority.expiresAt,
			"reset_priority_remaining_percent", priority.remainingPercent,
		)
	}
	return attrs
}

func logResponseUsage(log *slog.Logger, thread, account, model, serviceTier string, metadata turnMetadata, duration time.Duration, usage responseUsage) {
	log.Debug("response usage",
		"thread", thread,
		"turn", metadata.TurnID,
		"request_kind", metadata.RequestKind,
		"account", account,
		"model", model,
		"service_tier", serviceTier,
		"duration", duration,
		"input_tokens", usage.InputTokens,
		"cached_tokens", usage.InputDetails.CachedTokens,
		"cache_write_tokens", usage.InputDetails.CacheWriteTokens,
		"output_tokens", usage.OutputTokens,
		"reasoning_tokens", usage.OutputDetails.ReasoningTokens,
	)
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
