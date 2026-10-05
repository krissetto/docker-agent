package api

// WorkspaceSnapshots is owned by the executing session's runtime, not its viewer.
type WorkspaceSnapshots struct {
	SessionID string `json:"session_id"`
	Enabled   bool   `json:"enabled"`
	Files     []int  `json:"files"`
	Proof     string `json:"proof"`
}
type WorkspaceSnapshotRequest struct {
	ExpectedProof string `json:"expected_proof"`
	Keep          int    `json:"keep,omitempty"`
}
type WorkspaceSnapshotResult struct {
	SessionID     string `json:"session_id"`
	RestoredFiles int    `json:"restored_files"`
	Restored      bool   `json:"restored"`
}
