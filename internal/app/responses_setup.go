package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
)

// Initial handshakes and first-turn model preflight use the same mapping. The
// relay retains ownership of failed.Body until this adapter has consumed it.
func (d *httpResponsesDownstream) setupFailed(ctx context.Context, failed *http.Response, err error) error {
	failure := httpResponseFailure{Status: 502, Code: "upstream_unavailable", Message: "upstream connection unavailable; retry with full history"}
	if errors.Is(err, errNoAccountAvailable) || errors.Is(err, errRouteOwnerUnavailable) {
		failure.Status, failure.Code, failure.Message = 503, "route_unavailable", "no eligible account or route owner temporarily unavailable; retry"
	}
	if errors.Is(err, errAccountBoundTurn) {
		failure.Status, failure.Code, failure.Message = 409, "account_bound_request", errAccountBoundTurn.Error()
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		failure.Status, failure.Code = 504, "upstream_timeout"
	}
	var rejection *websocketSetupError
	if errors.As(err, &rejection) {
		details, _ := json.Marshal(rejection.details)
		failure = responseFailure(responseFields{"error": details}, rejection.status)
		if rejection.retryAfter != "" && !d.committed {
			d.writer.Header().Set("Retry-After", rejection.retryAfter)
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
		if !d.committed {
			copyHTTPResponseHeaders(d.writer.Header(), failed.Header)
		}
	}
	if ctx.Err() != nil {
		failure.Status, failure.Code, failure.Message = 503, "request_canceled", "request canceled or server shutting down"
	}
	return d.fail(failure)
}
