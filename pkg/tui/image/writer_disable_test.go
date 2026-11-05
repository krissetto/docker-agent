package image

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriterDisablingClearsExistingPlacements(t *testing.T) {
	for _, capability := range []bool{false, true} {
		t.Run(map[bool]string{false: "preference", true: "capability"}[capability], func(t *testing.T) {
			var output bytes.Buffer
			writer := NewWriter(&output)
			content := KittySequence([]byte("png-data"), 20, 10)
			writer.SetContent(content)
			_, err := writer.Write([]byte("first frame"))
			require.NoError(t, err)
			output.Reset()
			if capability {
				writer.SetSupported(false)
			} else {
				writer.SetEnabled(false)
			}
			// A hidden pane can retain its previous frame without calling SetContent.
			_, err = writer.Write([]byte("cached frame"))
			require.NoError(t, err)
			assert.Contains(t, output.String(), "a=d,d=a")
			assert.NotContains(t, output.String(), "a=p,i=")
			assert.NotContains(t, output.String(), "a=t,t=d")
			output.Reset()
			writer.SetSupported(true)
			writer.SetEnabled(true)
			writer.SetContent(content)
			_, err = writer.Write([]byte("enabled frame"))
			require.NoError(t, err)
			assert.Contains(t, output.String(), "a=p,i=")
		})
	}
}
