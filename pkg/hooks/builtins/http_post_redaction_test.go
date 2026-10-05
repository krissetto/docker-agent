package builtins

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type secretErrorDoer struct{ status int }

func (d secretErrorDoer) Do(req *http.Request) (*http.Response, error) {
	if d.status != 0 {
		return &http.Response{StatusCode: d.status, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: &url.Error{Op: "redirect", URL: req.URL.String(), Err: errors.New("fake transport error")}}
}

func TestHTTPPostLogsExcludePathAndQuerySecrets(t *testing.T) {
	for _, status := range []int{0, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(old)
			_, err := newHTTPPost(secretErrorDoer{status: status})(t.Context(), nil, []string{"https://example.com/PATH_SECRET?key=QUERY_SECRET", "{}"})
			require.NoError(t, err)
			require.NotEmpty(t, logs.String())
			for _, secret := range []string{"PATH_SECRET", "QUERY_SECRET"} {
				require.NotContains(t, logs.String(), secret)
			}
			require.Contains(t, logs.String(), "https://example.com")
		})
	}
}
