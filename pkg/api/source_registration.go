package api

const SourceRegistrationPath = "/api/v2/sources"

// SourceRegistrationRequest selects an accessible, workspace-relative YAML source.
type SourceRegistrationRequest struct {
	Path       string `json:"path"`
	WorkingDir string `json:"working_dir,omitempty"`
}

// SourceRegistration identifies an immutable revision of a stable logical source.
type SourceRegistration struct {
	SourceID string `json:"source_id"`
	Revision string `json:"revision"`
}
