package messages

import "github.com/docker/docker-agent/pkg/tui/image"

// OpenImagePreviewMsg transfers a cached image lease from a completed click.
// The receiver must either give it to the dialog or close it on rejection.
type OpenImagePreviewMsg struct {
	SessionID string
	Preview   *image.Preview
}
