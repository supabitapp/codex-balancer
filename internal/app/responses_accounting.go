package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type responseAccounting struct {
	server      *server
	request     *http.Request
	apiKey      apiKeyIdentity
	via         transport
	route       websocketRoute
	thread      string
	ctx         context.Context
	liveThreads map[string]struct{}
	turns       []websocketTurn
	account     *responseAccount
}

func (r *responseAccounting) startTurn(event websocketEnvelope) {
	metadata := requestTurnMetadata("", event.ClientMetadata)
	statsThread := statsThreadKey(r.thread, metadata)
	counted := event.Generate == nil || *event.Generate
	r.turns = append(r.turns, websocketTurn{
		sent:        time.Now(),
		model:       event.Model,
		effort:      event.Reasoning.Effort,
		serviceTier: event.ServiceTier,
		metadata:    metadata,
		counted:     counted,
		turnState:   event.ClientMetadata[codexTurnStateKey],
		statsThread: statsThread,
	})
	attrs := []any{"thread", statsThread, "service_tier", event.ServiceTier}
	attrs = append(attrs, routingLogAttrs(r.account.account.routingCandidate(), time.Now())...)
	r.server.log.Debug("response turn received", attrs...)
}

func (r *responseAccounting) responseCreated() bool {
	for index := range r.turns {
		if r.turns[index].created {
			continue
		}
		turn := &r.turns[index]
		routeThread := r.route.thread
		if routeThread == "" {
			routeThread = turn.statsThread
		}
		acceptedAt := time.Now()
		acceptance := r.server.acceptResponseRoute(r.account, storedRoute{At: acceptedAt, Session: r.route.session, Thread: routeThread, Account: r.account.account.id()})
		if !acceptance.allowed {
			return false
		}
		r.turns[index].created = true
		if observed := observation(r.ctx); observed != nil {
			observed.accepted, observed.account = true, r.account.account.id()
			attrs := observed.safe([]attribute.KeyValue{attribute.String("account", observed.account), attribute.String("prior_owner", r.account.priorOwner), attribute.String("routing_reason", string(r.account.routingReason)), attribute.Bool("accepted_switch", acceptance.logSwitch), attribute.Bool("route_persisted", acceptance.persisted)})
			observed.root.SetAttributes(attrs...)
			trace.SpanFromContext(r.ctx).SetAttributes(attrs...)
			observed.emit(r.ctx, slog.LevelInfo, "response_accepted", true, attribute.String("account", observed.account), attribute.String("prior_owner", r.account.priorOwner), attribute.String("routing_reason", string(r.account.routingReason)), attribute.Bool("accepted_switch", acceptance.logSwitch), attribute.Bool("route_persisted", acceptance.persisted), attribute.Bool("write_succeeded", observed.writeSucceeded), attribute.Int64("accept_latency_ms", time.Since(turn.sent).Milliseconds()))
		}
		if turn.counted {
			if _, live := r.liveThreads[turn.statsThread]; !live {
				r.server.stats.activateThread(turn.statsThread)
				r.liveThreads[turn.statsThread] = struct{}{}
			}
		}
		r.server.stats.recordAccepted(acceptedAt, turn.statsThread, requestIP(r.request), r.apiKey, r.account.account.id(), turn.model, turn.effort, turn.serviceTier, r.via, turn.metadata, turn.counted)
		if acceptance.logSwitch {
			r.server.log.Info("response account switch accepted",
				"thread", turn.statsThread,
				"from_account", r.account.priorOwner,
				"to_account", r.account.account.id(),
				"routing_reason", r.account.routingReason,
				"route_persisted", acceptance.persisted,
			)
		}
		r.account.moved = false
		if turn.counted {
			r.server.stats.answered(turn.statsThread, r.account.account.id(), time.Since(turn.sent))
		}
		r.server.log.Debug("response created", "thread", turn.statsThread, "turn", turn.metadata.TurnID, "account", r.account.account.id(), "latency", time.Since(turn.sent))
		return true
	}
	return true
}

func (r *responseAccounting) responseFinished(event websocketEnvelope) {
	if len(r.turns) == 0 {
		return
	}
	turn := r.turns[0]
	if turn.counted && event.Type != "error" {
		model := event.Response.Model
		if model == "" {
			model = turn.model
		}
		serviceTier := event.Response.ServiceTier
		if serviceTier == "" {
			serviceTier = turn.serviceTier
		}
		observation(r.ctx).usage(r.ctx, model, serviceTier, event.Type, event.Response.Usage)
		if !event.Response.Usage.empty() {
			logResponseUsage(r.server.log, turn.statsThread, r.account.account.id(), model, serviceTier, turn.metadata, time.Since(turn.sent), event.Response.Usage)
		}
		r.server.stats.recordAPIKeyUsage(r.apiKey.name, turn.statsThread, r.account.account.id(), model, turn.effort, serviceTier, event.Response.Usage)
		if event.Type == "response.completed" {
			r.server.stats.completed(turn.statsThread, r.account.account.id(), turn.metadata, time.Since(turn.sent))
		}
	}
	r.turns = r.turns[1:]
}
