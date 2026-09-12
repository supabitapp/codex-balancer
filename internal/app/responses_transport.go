package app

import (
	"context"
	"errors"
	"net/http"

	"github.com/coder/websocket"
)

var errResponseFinished = errors.New("response transport finished")

// The relay owns routing and turn state. A downstream owns only framing,
// validation and termination. HTTP supplies exactly one response.create.
type responsesDownstream interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
	prepare(websocketMessage) (websocketMessage, error)
	reject(context.Context, websocketMessage, websocket.StatusCode, string) error
	setupFailed(context.Context, *http.Response, error) error
}

type websocketDownstream struct{ *websocket.Conn }

func (d websocketDownstream) prepare(message websocketMessage) (websocketMessage, error) {
	return message, nil
}

func (d websocketDownstream) setupFailed(_ context.Context, _ *http.Response, _ error) error {
	return d.Close(websocket.StatusTryAgainLater, "no account supports requested model")
}

func (d websocketDownstream) reject(_ context.Context, _ websocketMessage, status websocket.StatusCode, reason string) error {
	return d.Close(status, reason)
}
