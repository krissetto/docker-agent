package api

const ServerInfoPath = "/api/v2/server"

// ServerInfo identifies one process and its configured workspace. Unmanaged
// servers may omit workspace, source and configuration fingerprint.
type ServerInfo struct {
	Version           int      `json:"version"`
	SessionAPIVersion int      `json:"session_api_version"`
	InstanceID        string   `json:"instance_id"`
	Ready             bool     `json:"ready"`
	WorkspaceRoot     string   `json:"workspace_root,omitempty"`
	Source            string   `json:"source,omitempty"`
	ConfigFingerprint string   `json:"config_fingerprint,omitempty"`
	Capabilities      []string `json:"capabilities"`
}
