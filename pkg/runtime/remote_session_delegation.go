package runtime

import (
	"context"
	"net/http"
)

func (s *remoteSession) DelegationPolicy(ctx context.Context) (bool, error) {
	if !s.Metadata().Capabilities.DelegationPolicy {
		return false, sessionUnsupported(s.ID(), "delegation_policy")
	}
	var result struct {
		Enabled bool `json:"enabled"`
	}
	if err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("delegation-policy"), nil, &result); err != nil {
		return false, err
	}
	return result.Enabled, nil
}

func (s *remoteSession) SetDelegationPolicy(ctx context.Context, enabled bool) error {
	if !s.Metadata().Capabilities.DelegationPolicy {
		return sessionUnsupported(s.ID(), "delegation_policy")
	}
	return s.runtime.client.sessionJSON(ctx, http.MethodPatch, s.endpoint("delegation-policy"), struct {
		Enabled bool `json:"enabled"`
	}{enabled}, nil)
}
