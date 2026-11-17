package oaistream

import (
	"encoding/base64"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDecodeImage(t *testing.T) {
	for _, raw := range []string{"https://example.com/private.png", "not base64!", "data:text/html;base64,PGgxPg==", "data:image/png;base64,aGVsbG8=", "data:image/png,raw"} {
		_, err := DecodeImage(raw)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), raw)
	}
}

func TestChatImageOutput(t *testing.T) {
	data := []byte("\x89PNG\r\n\x1a\nimage bytes")
	encoded := base64.StdEncoding.EncodeToString(data)
	stream := newTestStream(t, fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"images\":[{\"type\":\"image_url\",\"image_url\":{\"url\":\"data:image/png;base64,%s\"}}]}}]}\n\ndata: [DONE]\n\n", encoded))
	adapter := NewStreamAdapter(stream, false, true)
	defer adapter.Close()
	response, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, response.Choices[0].Delta.Media, 1)
	assert.Equal(t, data, response.Choices[0].Delta.Media[0].Data)
	assert.Empty(t, response.Choices[0].Delta.Content)
}

func TestChatRejectsRemoteImage(t *testing.T) {
	stream := newTestStream(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"images\":[{\"image_url\":{\"url\":\"http://localhost/secret\"}}]}}]}\n\n")
	adapter := NewStreamAdapter(stream, false, true)
	defer adapter.Close()
	_, err := adapter.Recv()
	require.ErrorContains(t, err, "inline image data")
}

func TestChatImageOutputDisabledByDefault(t *testing.T) {
	stream := newTestStream(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"images\":[{\"image_url\":{\"url\":\"http://localhost/private\"}}]}}]}\n\n")
	adapter := NewStreamAdapter(stream, false)
	defer adapter.Close()
	response, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, response.Choices, 1)
	assert.Empty(t, response.Choices[0].Delta.Media)
}
