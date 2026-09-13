package server

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	docMethodPattern = regexp.MustCompile("`(GET|POST|PATCH|DELETE|PUT)`")
	docPathPattern   = regexp.MustCompile("`(/[^`? ]+)(?:\\?[^`]*)?`")
)

func TestAPIServerMarkdownDocumentsEveryRoute(t *testing.T) {
	markdown, err := os.ReadFile("../../docs/features/api-server/index.md")
	require.NoError(t, err)
	_, endpointSection, found := strings.Cut(string(markdown), "## Endpoints\n")
	require.True(t, found, "API server markdown must contain an Endpoints section")
	endpointSection, _, found = strings.Cut(endpointSection, "## Workflow summary")
	require.True(t, found, "API server Endpoints section must end before Workflow summary")

	documented := make(map[string]struct{})
	for line := range strings.SplitSeq(endpointSection, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		methods := docMethodPattern.FindAllStringSubmatch(line, -1)
		paths := docPathPattern.FindAllStringSubmatch(line, -1)
		if len(methods) == 0 || len(paths) == 0 {
			continue
		}
		switch {
		case len(methods) == 1:
			for _, path := range paths {
				documented[methods[0][1]+" "+path[1]] = struct{}{}
			}
		case len(paths) == 1:
			for _, method := range methods {
				documented[method[1]+" "+paths[0][1]] = struct{}{}
			}
		case len(methods) == len(paths):
			for i := range methods {
				documented[methods[i][1]+" "+paths[i][1]] = struct{}{}
			}
		default:
			t.Fatalf("ambiguous endpoint table row: %s", line)
		}
	}

	server := NewWithManager(nil, "")
	registered := make(map[string]struct{})
	for _, route := range server.e.Routes() {
		if route.Method == http.MethodHead || route.Method == http.MethodOptions {
			continue
		}
		registered[route.Method+" "+route.Path] = struct{}{}
	}

	for route := range registered {
		assert.Contains(t, documented, route, "registered route is missing from API server markdown")
	}
	for route := range documented {
		assert.Contains(t, registered, route, "documented route %q is not registered", route)
	}
}
