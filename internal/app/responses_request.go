package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// RawMessage is intentional: the routing envelope is not a lossless request.
type responseFields map[string]json.RawMessage

func responseObject(data []byte) (responseFields, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("JSON must be UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	fields := responseFields{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid object key")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		fields[key] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("unexpected trailing JSON")
	}
	return fields, nil
}

func responseString(raw json.RawMessage) (string, bool) {
	var text string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

func validateHTTPResponse(data []byte) (websocketEnvelope, bool, error) {
	fields, err := responseObject(data)
	if err != nil {
		return websocketEnvelope{}, false, err
	}
	for _, key := range []string{"model", "stream", "reasoning", "service_tier", "client_metadata", "previous_response_id"} {
		for field := range fields {
			if field != key && strings.EqualFold(field, key) {
				return websocketEnvelope{}, false, fmt.Errorf("use the field spelling %q", key)
			}
		}
	}
	model, ok := responseString(fields["model"])
	if !ok || strings.TrimSpace(model) == "" {
		return websocketEnvelope{}, false, errors.New("model must be a non-empty string")
	}
	stream := false
	if raw, exists := fields["stream"]; exists {
		if string(raw) != "true" && string(raw) != "false" {
			return websocketEnvelope{}, false, errors.New("stream must be a boolean")
		}
		stream = string(raw) == "true"
	}
	var request struct {
		Model              string            `json:"model"`
		Reasoning          responseReasoning `json:"reasoning"`
		Input              responseInput     `json:"input"`
		ServiceTier        string            `json:"service_tier"`
		PreviousResponseID string            `json:"previous_response_id"`
		ClientMetadata     map[string]string `json:"client_metadata"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return websocketEnvelope{}, false, errors.New("invalid routing fields")
	}
	return websocketEnvelope{
		Model: request.Model, Reasoning: request.Reasoning, Input: request.Input, ServiceTier: request.ServiceTier,
		PreviousResponseID: request.PreviousResponseID, ClientMetadata: request.ClientMetadata,
	}, stream, nil
}
