package main

import (
	"log/slog"
	"time"
)

func (s *server) pickAccount(thread, required, preferred, model, serviceTier string, skip map[string]bool, attempt int, via transport) *Account {
	allowed := s.allowedAccounts(model, serviceTier)
	decision := s.pool.route(required, preferred, skip, allowed)
	s.log.Debug("routing attempt",
		"transport", via,
		"thread", thread,
		"attempt", attempt+1,
		"required_account", required,
		"preferred_account", preferred,
		"model", model,
		"service_tier", serviceTier,
		"accounts", len(decision.candidates),
	)
	for _, candidate := range decision.candidates {
		attrs := []any{
			"transport", via,
			"thread", thread,
			"attempt", attempt + 1,
			"selected", candidate.account == decision.account,
			"required", candidate.id == required,
			"preferred", candidate.id == preferred,
			"skipped", skip[candidate.id],
			"model_eligible", accountAllowed(allowed, candidate.id),
		}
		attrs = append(attrs, routingLogAttrs(candidate, decision.now)...)
		s.log.Debug("routing candidate", attrs...)
	}
	if decision.account == nil {
		s.log.Warn("no account available",
			"transport", via,
			"thread", thread,
			"attempt", attempt+1,
			"required_account", required,
			"preferred_account", preferred,
			"model", model,
			"service_tier", serviceTier,
		)
	}
	return decision.account
}

func (s *server) allowedAccounts(model, serviceTier string) map[string]bool {
	if s.catalog == nil {
		return nil
	}
	return s.catalog.allowedAccounts(s.pool.all(), model, serviceTier)
}

func routingLogAttrs(candidate routingCandidate, now time.Time) []any {
	return []any{
		"account", candidate.id,
		"status", candidate.status(now),
		"reported_limit_reached", candidate.spent,
		"cooldown_until", candidate.cooldown,
		"last_used_at", candidate.lastUsed,
		"reauth", candidate.reauth,
		"primary", windowLogValue(candidate.primary),
		"secondary", windowLogValue(candidate.secondary),
	}
}

func logResponseUsage(log *slog.Logger, via transport, thread, account, model, serviceTier string, metadata turnMetadata, rotationSource string, compactionReplay bool, duration time.Duration, usage responseUsage) {
	log.Debug("response usage",
		"transport", via,
		"thread", thread,
		"turn", metadata.TurnID,
		"request_kind", metadata.RequestKind,
		"account", account,
		"rotation_source", rotationSource,
		"compaction_replay", compactionReplay,
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
