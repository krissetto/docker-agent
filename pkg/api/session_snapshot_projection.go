package api

import (
	"encoding/json"
)

// MarshalJSON omits Session.Messages only for explicitly transcript-free
// projections. Default snapshots preserve the historic full snapshot contract.
func (s SessionSnapshot[State, Kind, Event]) MarshalJSON() ([]byte, error) {
	type plain SessionSnapshot[State, Kind, Event]
	if s.Projection != SessionHistoryProjection && s.Projection != SessionViewMetadataProjection {
		return json.Marshal(plain(s))
	}
	if s.Session != nil && s.Session.ItemCount() != 0 {
		header, err := s.Session.HistoryHeaderSnapshot(4 << 20)
		if err != nil {
			return nil, err
		}
		s.Session = header
	}
	data, err := json.Marshal(plain(s))
	if err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(root["session"], &header); err != nil {
		return nil, err
	}
	delete(header, "messages")
	encoded, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	root["session"] = encoded
	return json.Marshal(root)
}
