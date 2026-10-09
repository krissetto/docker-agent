package root

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttachAuthorityAddressSafety(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		address, token string
		valid          bool
	}{
		{"http://127.0.0.1:8080", "", true},
		{"http://[::1]:8080", "", true},
		{"https://authority.example", "secret", true},
		{"https://authority.example", "", false},
		{"http://authority.example", "secret", false},
		{"https://secret@authority.example", "secret", false},
		{"https://authority.example?token=secret", "secret", false},
		{"https://authority.example#secret", "secret", false},
	} {
		t.Run(tc.address, func(t *testing.T) {
			t.Parallel()
			err := validateAttachAddress(tc.address, tc.token)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
