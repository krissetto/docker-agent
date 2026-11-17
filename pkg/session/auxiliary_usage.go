package session

import (
	"encoding/json"

	"github.com/docker/docker-agent/pkg/chat"
)

const auxiliaryUsageAttribute = "docker-agent.accounting.auxiliary"

// AuxiliaryUsage is aggregate accounting for requests that are not conversation
// turns. No request or response content is retained.
type AuxiliaryUsage struct {
	Agent string     `json:"agent"`
	Model string     `json:"model"`
	Cost  float64    `json:"cost"`
	Usage chat.Usage `json:"usage"`
}

func (s *Session) auxiliaryUsageLocked() []AuxiliaryUsage {
	var records []AuxiliaryUsage
	_ = json.Unmarshal([]byte(s.Attributes[auxiliaryUsageAttribute]), &records)
	return records
}

// AuxiliaryUsageSnapshot returns detached accounting metadata, not history.
func (s *Session) AuxiliaryUsageSnapshot() []AuxiliaryUsage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auxiliaryUsageLocked()
}

func (s *Session) AddAuxiliaryUsage(record AuxiliaryUsage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.auxiliaryUsageLocked()
	found := false
	for i := range records {
		previous := &records[i]
		if previous.Agent == record.Agent && previous.Model == record.Model {
			previous.Cost += record.Cost
			previous.Usage.Add(&record.Usage)
			found = true
			break
		}
	}
	if !found {
		records = append(records, record)
	}
	data, _ := json.Marshal(records)
	if s.Attributes == nil {
		s.Attributes = make(map[string]string)
	}
	s.Attributes[auxiliaryUsageAttribute] = string(data)
}

func (s *Session) auxiliaryCostLocked() float64 {
	var cost float64
	for _, record := range s.auxiliaryUsageLocked() {
		cost += record.Cost
	}
	return cost
}

// SetAuxiliaryUsage replaces a cumulative accounting snapshot received from the
// canonical owner. Replayed events are therefore idempotent.
func (s *Session) SetAuxiliaryUsage(records []AuxiliaryUsage) {
	data, _ := json.Marshal(records)
	s.SetAttribute(auxiliaryUsageAttribute, string(data))
}
