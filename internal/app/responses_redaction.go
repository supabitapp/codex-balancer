package app

import (
	"bytes"
	"encoding/json"
	"strings"
)

func (d *httpResponsesDownstream) redactor() *strings.Replacer {
	secrets := append([]string(nil), d.secrets...)
	for _, account := range d.accounts {
		state := account.persisted()
		secrets = append(secrets, state.AccessToken, state.RefreshToken, state.IDToken)
	}
	seen := map[string]bool{}
	pairs := make([]string, 0, 2*len(secrets))
	for _, secret := range secrets {
		if secret != "" && !seen[secret] {
			seen[secret] = true
			pairs = append(pairs, secret, "[redacted]")
		}
	}
	return strings.NewReplacer(pairs...)
}

func (d *httpResponsesDownstream) redact(data []byte) []byte {
	return redactJSONStrings(data, d.redactor())
}

// Input is validated JSON. Decode only string tokens (including object keys),
// and re-encode only those that change. Numbers and unrelated RawMessage data
// retain their exact spelling, without a float64/object-tree round trip.
func redactJSONStrings(data []byte, replacer *strings.Replacer) []byte {
	var result []byte
	copied := 0
	for index := 0; index < len(data); index++ {
		if data[index] != '"' {
			continue
		}
		start := index
		index++
		for index < len(data) && data[index] != '"' {
			if data[index] == '\\' {
				index++
			}
			index++
		}
		if index >= len(data) {
			break
		}
		token := data[start : index+1]
		text := string(token[1 : len(token)-1])
		if bytes.IndexByte(token, '\\') >= 0 {
			if json.Unmarshal(token, &text) != nil {
				continue
			}
		}
		redacted := replacer.Replace(text)
		if redacted == text {
			continue
		}
		result = append(result, data[copied:start]...)
		encoded, _ := json.Marshal(redacted)
		result = append(result, encoded...)
		copied = index + 1
	}
	if result == nil {
		return data
	}
	return append(result, data[copied:]...)
}
