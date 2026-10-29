package messages

import (
	"github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// OpenImagePreviewMsg transfers a cached image lease from a completed click.
// The receiver must either give it to the dialog or close it on rejection.
type OpenImagePreviewMsg struct {
	SessionID string
	Preview   *image.Preview
}

// OpenUserAttachmentPreviewMsg requests an explicit preview of bytes/text
// already stored on a user message. Source paths are provenance, never read here.
type OpenUserAttachmentPreviewMsg struct {
	SessionID  string
	Attachment types.UserAttachment
}
