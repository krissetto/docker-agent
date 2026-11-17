package oaistream

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
)

// DecodeImage decodes an inline provider image. Remote URLs are deliberately not
// fetched: provider output is untrusted and must not initiate arbitrary requests.
func DecodeImage(encoded string) (chat.MediaDelta, error) {
	if strings.HasPrefix(encoded, "data:") {
		header, body, ok := strings.Cut(encoded, ",")
		if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
			return chat.MediaDelta{}, errors.New("unsupported image data URL (expected base64 image)")
		}
		encoded = body
	} else if strings.Contains(encoded, "://") {
		return chat.MediaDelta{}, errors.New("remote image URLs are unsupported; request b64_json or inline image data")
	}
	const maxImageBytes = 64 * 1024 * 1024
	if base64.StdEncoding.DecodedLen(len(encoded)) > maxImageBytes {
		return chat.MediaDelta{}, errors.New("generated image exceeds 64 MiB limit")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return chat.MediaDelta{}, errors.New("invalid base64 generated image")
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return chat.MediaDelta{}, fmt.Errorf("generated image has unsupported content type %s", mime)
	}
	return chat.MediaDelta{Data: data, MimeType: mime, Size: int64(len(data))}, nil
}
