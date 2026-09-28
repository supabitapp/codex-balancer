package app

import "encoding/json"

// Upstream overloads top-level status: error events use numeric HTTP codes,
// while response events can use lifecycle strings. The accounting envelope
// needs only the HTTP code; forwarding uses the original event, not this value.
type responseEventStatus int

func (s *responseEventStatus) UnmarshalJSON(data []byte) error {
	var code int
	if len(data) > 0 && data[0] == '"' {
		var lifecycle string
		if err := json.Unmarshal(data, &lifecycle); err != nil {
			return err
		}
	} else if err := json.Unmarshal(data, &code); err != nil {
		return err
	}
	*s = responseEventStatus(code)
	return nil
}
