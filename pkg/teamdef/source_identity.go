package teamdef

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
)

// SourceIdentity returns a stable, credential-free fallback team identity for
// sources without an explicit configuration ID. URL query, fragment and user
// information are omitted before hashing; local source names retain their exact
// path semantics and only the digest is persisted.
func SourceIdentity(sourceName string) string {
	identity := sourceName
	if parsed, err := url.Parse(sourceName); err == nil && parsed.Scheme != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		identity = parsed.String()
	}
	identity = strings.TrimSpace(identity)
	sum := sha256.Sum256([]byte(identity))
	return "source:" + hex.EncodeToString(sum[:])
}
