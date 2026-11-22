package gemini

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// The genai SDK reads GOOGLE_GEMINI_BASE_URL from the process environment when no
// endpoint is configured, so an unset model endpoint leaves the override to the SDK.
func TestNewClientHonorsGoogleGeminiBaseURLEnv(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"models/gemini-2.5-flash"}`))
	}))
	defer srv.Close()
	t.Setenv("GOOGLE_GEMINI_BASE_URL", srv.URL+"/")

	cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash"}
	client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"GOOGLE_API_KEY": "test-key"}))
	require.NoError(t, err)

	sdk, err := client.clientFn(t.Context())
	require.NoError(t, err)
	_, err = sdk.Models.Get(t.Context(), "gemini-2.5-flash", nil)
	require.NoError(t, err)
	assert.Contains(t, gotPath, "gemini-2.5-flash", "request must reach the GOOGLE_GEMINI_BASE_URL endpoint")
}
