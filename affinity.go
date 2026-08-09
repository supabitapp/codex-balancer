package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type affinityKind string

const (
	affinitySession      affinityKind = "session"
	affinityPromptCache  affinityKind = "prompt_cache"
	affinityTurnState    affinityKind = "turn_state"
	affinityResponse     affinityKind = "response"
	affinityConversation affinityKind = "conversation"
	affinityFile         affinityKind = "file"
)

var (
	errAffinityConflict         = errors.New("affinity owners conflict")
	errAffinityOwnerUnavailable = errors.New("affinity owner unavailable")
	errAffinityAmbiguous        = errors.New("affinity owner is ambiguous")
)

type affinityRef struct {
	kind  affinityKind
	value string
}

func (r affinityRef) valid() bool {
	return r.kind != "" && r.value != ""
}

func (r affinityRef) hard() bool {
	switch r.kind {
	case affinityTurnState, affinityResponse, affinityConversation, affinityFile:
		return true
	default:
		return false
	}
}

func (r affinityRef) abandonable() bool {
	return r.kind == affinityTurnState || r.kind == affinityConversation
}

func (r affinityRef) storageKey() string {
	return string(r.kind) + "\n" + r.value
}

type requestAffinity struct {
	preferred          affinityRef
	hard               []affinityRef
	requireUnambiguous bool
}

func (a requestAffinity) bindings() []affinityRef {
	seen := map[affinityRef]bool{}
	bindings := make([]affinityRef, 0, len(a.hard)+1)
	if a.preferred.valid() {
		seen[a.preferred] = true
		bindings = append(bindings, a.preferred)
	}
	for _, ref := range a.hard {
		if !ref.valid() || seen[ref] {
			continue
		}
		seen[ref] = true
		bindings = append(bindings, ref)
	}
	return bindings
}

func (a requestAffinity) statsKey(headers http.Header) string {
	if session := sessionAffinity(headers); session.valid() {
		return session.value
	}
	if a.preferred.valid() {
		return a.preferred.value
	}
	if len(a.hard) > 0 {
		return a.hard[0].value
	}
	return ""
}

func hardAffinityRefs(refs []affinityRef) []affinityRef {
	hard := make([]affinityRef, 0, len(refs))
	for _, ref := range refs {
		if ref.hard() {
			hard = append(hard, ref)
		}
	}
	return hard
}

type affinityResolution struct {
	required  string
	preferred string
	bindings  []affinityRef
	hard      bool
}

func affinityFromRequest(headers http.Header, body []byte) (requestAffinity, error) {
	var payload map[string]any
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			return requestAffinity{}, err
		}
	}

	var affinity requestAffinity
	turnState := clientTurnStateAffinity(payload)
	if !turnState.valid() {
		if value := firstHeader(headers, "x-codex-turn-state"); value != "" {
			turnState = affinityRef{kind: affinityTurnState, value: value}
		}
	}
	if turnState.valid() {
		affinity.hard = append(affinity.hard, turnState)
	} else if session := sessionAffinity(headers); session.valid() {
		affinity.preferred = session
	} else {
		cache, ok := nonEmptyString(payload["prompt_cache_key"])
		if !ok {
			cache, ok = nonEmptyString(payload["promptCacheKey"])
		}
		if ok {
			affinity.preferred = affinityRef{kind: affinityPromptCache, value: cache}
		}
	}

	if previous, ok := nonEmptyString(payload["previous_response_id"]); ok {
		affinity.hard = append(affinity.hard, affinityRef{kind: affinityResponse, value: previous})
	}
	if conversation, ok := nonEmptyString(payload["conversation"]); ok {
		affinity.hard = append(affinity.hard, affinityRef{kind: affinityConversation, value: conversation})
		affinity.requireUnambiguous = true
	} else if value, exists := payload["conversation"]; exists && value != nil {
		if _, ok := value.(string); !ok {
			affinity.requireUnambiguous = true
		}
	}
	for _, fileID := range inputFileIDs(payload["input"]) {
		affinity.hard = append(affinity.hard, affinityRef{kind: affinityFile, value: fileID})
	}
	return affinity, nil
}

func turnStateAffinity(headers http.Header) affinityRef {
	value := firstHeader(headers, "x-codex-turn-state")
	if value == "" {
		return affinityRef{}
	}
	return affinityRef{kind: affinityTurnState, value: value}
}

func clientTurnStateAffinity(payload map[string]any) affinityRef {
	metadata, ok := payload["client_metadata"].(map[string]any)
	if !ok {
		return affinityRef{}
	}
	value, ok := nonEmptyString(metadata["x-codex-turn-state"])
	if !ok {
		return affinityRef{}
	}
	return affinityRef{kind: affinityTurnState, value: value}
}

func sessionAffinity(headers http.Header) affinityRef {
	value := firstHeader(
		headers,
		"session_id",
		"session-id",
		"x-codex-session-id",
		"x-codex-conversation-id",
		"thread-id",
	)
	if value == "" {
		return affinityRef{}
	}
	return affinityRef{kind: affinitySession, value: value}
}

func firstHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func nonEmptyString(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	return text, text != ""
}

func inputFileIDs(value any) []string {
	var ids []string
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case []any:
			for _, item := range value {
				visit(item)
			}
		case map[string]any:
			if kind, _ := nonEmptyString(value["type"]); kind == "input_file" {
				if id, ok := nonEmptyString(value["file_id"]); ok {
					ids = append(ids, id)
				}
			}
			for _, item := range value {
				visit(item)
			}
		}
	}
	visit(value)
	return ids
}

func affinityErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, errAffinityConflict):
		return http.StatusBadGateway, "account-owned affinity sources conflict"
	case errors.Is(err, errAffinityOwnerUnavailable):
		return http.StatusServiceUnavailable, "account-owned affinity is unavailable"
	case errors.Is(err, errAffinityAmbiguous):
		return http.StatusServiceUnavailable, "account-owned affinity is ambiguous"
	default:
		return http.StatusBadRequest, fmt.Sprintf("invalid affinity: %v", err)
	}
}
