package main

import "encoding/json"

const (
	codexTurnMetadataKey = "x-codex-turn-metadata"
	codexTurnStateKey    = "x-codex-turn-state"
)

type turnMetadata struct {
	RequestKind        string             `json:"request_kind,omitempty"`
	ThreadID           string             `json:"thread_id,omitempty"`
	TurnID             string             `json:"turn_id,omitempty"`
	WindowID           string             `json:"window_id,omitempty"`
	ParentThreadID     string             `json:"parent_thread_id,omitempty"`
	ParentTurnID       string             `json:"parent_turn_id,omitempty"`
	ForkedFromThreadID string             `json:"forked_from_thread_id,omitempty"`
	SubagentKind       string             `json:"subagent_kind,omitempty"`
	Compaction         compactionMetadata `json:"compaction,omitempty"`
}

type compactionMetadata struct {
	Trigger string `json:"trigger,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Phase   string `json:"phase,omitempty"`
}

func requestTurnMetadata(header string, clientMetadata map[string]string) turnMetadata {
	value := clientMetadata[codexTurnMetadataKey]
	if header != "" {
		value = header
	}
	var metadata turnMetadata
	json.Unmarshal([]byte(value), &metadata)
	return metadata
}

func encodeTurnMetadata(metadata turnMetadata) string {
	value, _ := json.Marshal(metadata)
	return string(value)
}

func decodeTurnMetadata(value string) turnMetadata {
	var metadata turnMetadata
	json.Unmarshal([]byte(value), &metadata)
	return metadata
}

func statsThreadKey(route string, metadata turnMetadata) string {
	if metadata.ThreadID != "" {
		return metadata.ThreadID
	}
	return route
}
