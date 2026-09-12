package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
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

func translateHTTPResponse(data []byte) ([]byte, bool, error) {
	fields, err := responseObject(data)
	if err != nil {
		return nil, false, err
	}
	// Go's typed routing envelope accepts case-insensitive keys. Do not let an
	// unrecognized spelling override the fields we validated or injected.
	known := []string{"model", "input", "instructions", "stream", "store", "background", "previous_response_id", "conversation", "type", "generate", "response", "status", "status_code", "headers", "reasoning", "service_tier", "client_metadata"}
	for key := range fields {
		for _, canonical := range known {
			if key != canonical && strings.EqualFold(key, canonical) {
				return nil, false, fmt.Errorf("use the field spelling %q", canonical)
			}
		}
	}
	model, ok := responseString(fields["model"])
	if !ok || strings.TrimSpace(model) == "" {
		return nil, false, errors.New("model must be a non-empty string")
	}
	stream := false
	for _, key := range []string{"stream", "store", "background"} {
		if raw, exists := fields[key]; exists {
			if string(raw) != "true" && string(raw) != "false" {
				return nil, false, fmt.Errorf("%s must be a boolean", key)
			}
			value := string(raw) == "true"
			if key == "stream" {
				stream = value
			} else if value {
				return nil, false, fmt.Errorf("%s:true is unsupported; HTTP responses are stateless", key)
			}
		}
	}
	for _, key := range []string{"previous_response_id", "conversation"} {
		if raw, exists := fields[key]; exists && string(raw) != "null" {
			return nil, false, fmt.Errorf("%s is unsupported; send full input history", key)
		}
		delete(fields, key)
	}
	for _, key := range []string{"type", "generate", "response", "status", "status_code", "headers", "stream_options"} {
		if _, exists := fields[key]; exists {
			return nil, false, fmt.Errorf("%s is not supported by the HTTP adapter", key)
		}
	}
	// OpenCode's normal SDK and auxiliary generateObject calls may supply these
	// API generation knobs. Codex's request contract has no equivalent. Accept
	// well-formed values as compatibility hints, not enforceable output limits.
	for _, key := range []string{"max_output_tokens", "temperature", "top_p"} {
		if raw, exists := fields[key]; exists {
			value, err := strconv.ParseFloat(string(raw), 64)
			valid := err == nil && value >= 0
			switch key {
			case "max_output_tokens":
				integer, err := strconv.ParseInt(string(raw), 10, 64)
				valid = err == nil && integer > 0
			case "temperature":
				valid = valid && value <= 2
			case "top_p":
				valid = valid && value <= 1
			}
			if !valid {
				return nil, false, fmt.Errorf("invalid %s", key)
			}
			delete(fields, key)
		}
	}
	instructions := []string{}
	if raw, exists := fields["instructions"]; exists {
		text, ok := responseString(raw)
		if !ok {
			return nil, false, errors.New("instructions must be a string")
		}
		instructions = append(instructions, text)
	}
	input := []json.RawMessage{}
	if raw, exists := fields["input"]; exists {
		if text, ok := responseString(raw); ok {
			content, _ := json.Marshal([]map[string]string{{"type": "input_text", "text": text}})
			message, _ := json.Marshal(responseFields{"role": json.RawMessage(`"user"`), "content": content})
			input = append(input, message)
		} else if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &input) != nil {
			return nil, false, errors.New("input must be a string or item array")
		}
	}
	leading := true
	remaining := make([]json.RawMessage, 0, len(input))
	for _, raw := range input {
		item, err := responseObject(raw)
		if err != nil {
			return nil, false, errors.New("input items must be JSON objects with unique fields")
		}
		kind, kindOK := responseString(item["type"])
		if item["type"] != nil && (!kindOK || kind == "") {
			return nil, false, errors.New("input item type must be a non-empty string")
		}
		if kind == "item_reference" || kind == "" && item["id"] != nil && item["role"] == nil {
			return nil, false, errors.New("stored input references are unsupported; send full input items")
		}
		role, roleOK := responseString(item["role"])
		if item["role"] != nil && !roleOK {
			return nil, false, errors.New("input message role must be a string")
		}
		if kind == "message" || kind == "" && roleOK {
			if role != "user" && role != "assistant" && role != "system" && role != "developer" {
				return nil, false, errors.New("unsupported input message role")
			}
			if _, ok := responseString(item["content"]); !ok {
				var parts []json.RawMessage
				raw := item["content"]
				if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &parts) != nil {
					return nil, false, errors.New("message content must be a string or part array")
				}
				for _, raw := range parts {
					if _, err := responseObject(raw); err != nil {
						return nil, false, errors.New("message content parts must be objects")
					}
				}
			}
		}
		if leading && (kind == "" || kind == "message") && (role == "system" || role == "developer") {
			for key := range item {
				if key != "type" && key != "role" && key != "content" {
					return nil, false, fmt.Errorf("cannot lift instruction message field %q without losing semantics", key)
				}
			}
			text, err := instructionText(item["content"])
			if err != nil {
				return nil, false, err
			}
			instructions = append(instructions, text)
			continue
		}
		leading = false
		remaining = append(remaining, raw)
	}
	fields["input"], _ = json.Marshal(remaining)
	fields["instructions"], _ = json.Marshal(strings.Join(instructions, "\n\n"))
	fields["store"] = json.RawMessage("false")
	fields["type"] = json.RawMessage(`"response.create"`)
	delete(fields, "stream")
	delete(fields, "background")
	result, err := json.Marshal(fields)
	if err != nil {
		return nil, false, err
	}
	if raw, exists := fields["client_metadata"]; exists {
		if _, err := responseObject(raw); err != nil {
			return nil, false, errors.New("client_metadata must be an object with unique keys")
		}
	}
	// Validate precisely the fields needed by the shared router, without using
	// this lightweight envelope to serialize the request.
	var event websocketEnvelope
	if err := json.Unmarshal(result, &event); err != nil {
		return nil, false, errors.New("invalid routing fields: reasoning, service_tier or client_metadata")
	}
	return result, stream, nil
}

func instructionText(raw json.RawMessage) (string, error) {
	if text, ok := responseString(raw); ok {
		return text, nil
	}
	var parts []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &parts) != nil {
		return "", errors.New("initial system/developer content must be text")
	}
	texts := make([]string, 0, len(parts))
	for _, raw := range parts {
		part, err := responseObject(raw)
		kind, _ := responseString(part["type"])
		if err != nil || len(part) != 2 || kind != "input_text" {
			return "", errors.New("initial instruction parts support only type:input_text and text")
		}
		text, ok := responseString(part["text"])
		if !ok {
			return "", errors.New("initial instruction text must be a string")
		}
		texts = append(texts, text)
	}
	return strings.Join(texts, "\n\n"), nil
}
