package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
)

// Initial handshakes and first-turn model preflight use the same mapping. The
// relay retains ownership of failed.Body until this adapter has consumed it.
func responseSetupFailure(ctx context.Context, failed *http.Response, err error) (httpResponseFailure, http.Header) {
	headers := http.Header{}
	failure := httpResponseFailure{Status: 502, Code: "upstream_unavailable", Message: "upstream connection unavailable; retry with full history"}
	if errors.Is(err, errNoAccountAvailable) || errors.Is(err, errRouteOwnerUnavailable) {
		failure.Status, failure.Code, failure.Message = 503, "route_unavailable", "no eligible account or route owner temporarily unavailable; retry"
	}
	if errors.Is(err, errAccountBoundTurn) {
		failure = httpResponseFailure{Status: 400, Code: "account_bound_request", Type: "invalid_request_error", Message: errAccountBoundTurn.Error()}
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		failure.Status, failure.Code, failure.Message = 504, "upstream_timeout", "upstream connection timed out"
	}
	if errors.Is(err, errUpgradeBudget) {
		failure.Status, failure.Code, failure.Message = 503, "upgrade_timeout", errUpgradeBudget.Error()
	}
	var rejection *websocketSetupError
	if errors.As(err, &rejection) {
		details, _ := json.Marshal(rejection.details)
		failure = responseFailure(responseFields{"error": details}, rejection.status)
		if rejection.retryAfter != "" {
			headers.Set("Retry-After", rejection.retryAfter)
		}
	}
	if failed != nil {
		failure = httpResponseFailure{Status: failed.StatusCode, Code: "upstream_rejected", Message: "upstream rejected connection"}
		if failed.Body != nil {
			data, err := io.ReadAll(io.LimitReader(failed.Body, maxUpstreamErrorBody+1))
			if err == nil && len(data) <= maxUpstreamErrorBody {
				if fields, err := responseObject(data); err == nil {
					failure = responseFailure(fields, failed.StatusCode)
				}
			}
		}
		copyHTTPResponseHeaders(headers, failed.Header)
	}
	var unavailable *routeUnavailableError
	if errors.As(err, &unavailable) {
		// A previous attempt's rejection may concern a different account.
		// The current routing decision supplies the actual recovery deadline.
		headers.Set("Retry-After", retryAfterSeconds(unavailable.retryAt))
	}
	if ctx.Err() != nil {
		failure.Status, failure.Code, failure.Message = 503, "request_canceled", "request canceled or server shutting down"
	}
	upstreamStatus, mappedStatus := 0, failure.Status
	if failed != nil {
		upstreamStatus = failed.StatusCode
	} else if rejection != nil {
		upstreamStatus = rejection.status
	}
	if mappedStatus < 400 || mappedStatus > 599 {
		mappedStatus = 502
	}
	observation(ctx).event(ctx, "setup_failed", attribute.Int("upstream_status", upstreamStatus), attribute.Int("mapped_status", mappedStatus), attribute.String("error_code", failure.Code), attribute.String("retry_after", headers.Get("Retry-After")), attribute.String("error_type", telemetryErrorClass(err)), attribute.Bool("inference_sent", false))
	failure.Status = mappedStatus
	failure.UpstreamStatus = upstreamStatus
	return failure, headers
}

func (d *httpResponsesDownstream) setupFailed(ctx context.Context, failed *http.Response, err error) error {
	failure, headers := responseSetupFailure(ctx, failed, err)
	return d.writeSetupFailure(failure, headers)
}

func (d *httpResponsesDownstream) writeSetupFailure(failure httpResponseFailure, headers http.Header) error {
	if !d.committed {
		redactor := d.redactor()
		for name, values := range headers {
			for _, value := range values {
				d.writer.Header().Add(name, redactor.Replace(value))
			}
		}
	}
	return d.fail(failure)
}
