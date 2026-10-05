package builtins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/httpclient"
)

// HTTPPost is the registered name of the http_post builtin.
const HTTPPost = "http_post"

// httpDoer is the subset of *http.Client httpPost depends on. Narrowing to
// an interface lets Register inject an alternative client (e.g. the unsafe
// variant tests need to reach httptest.NewServer on 127.0.0.1) without any
// package-level mutable state.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// defaultHTTPPostClient is the production HTTP client used by httpPost. It
// refuses connections to non-public IPs at dial time (defeating DNS
// rebinding to loopback / RFC1918 / link-local incl. cloud metadata at
// 169.254.169.254) and bounds redirects at 10 hops.
func defaultHTTPPostClient() httpDoer {
	return httpclient.NewSafeClient(30*time.Second, false)
}

// newHTTPPost returns the http_post builtin bound to client. POSTs args[1]
// to args[0] with Content-Type: application/json.
//
// An empty URL is a no-op (lenient args contract). A non-http(s) or
// otherwise unparseable URL surfaces as an error so on_error: warn
// flags the misconfig. Network errors and non-2xx responses are
// logged (without URL paths, queries or credentials) and swallowed so a bad webhook
// never breaks the run loop. The hook executor already wraps ctx with
// [Hook.GetTimeout]; the client's Timeout is a backstop.
func newHTTPPost(client httpDoer) hooks.BuiltinFunc {
	return func(ctx context.Context, _ *hooks.Input, args []string) (*hooks.Output, error) {
		if len(args) == 0 || args[0] == "" {
			return nil, nil
		}
		target, err := url.Parse(args[0])
		if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
			return nil, errors.New("http_post: only http(s) URLs are supported")
		}
		var body string
		if len(args) >= 2 {
			body = args[1]
		}
		destination := target.Scheme + "://" + target.Host

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), strings.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("http_post: build request: %w", withoutURL(err))
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			slog.WarnContext(ctx, "http_post: request failed", "destination", destination, "error", withoutURL(err))
			return nil, nil
		}
		defer resp.Body.Close()
		// Cap the drain so a malicious receiver can't pin the goroutine on
		// an unbounded read; 64 KiB is plenty for a webhook ack.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode >= 400 {
			slog.WarnContext(ctx, "http_post: non-success response", "destination", destination, "status", resp.StatusCode)
		}
		return nil, nil
	}
}

func withoutURL(err error) error {
	var urlErr *url.Error
	for errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return err
}
