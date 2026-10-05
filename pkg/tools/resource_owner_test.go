package tools

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResourceOwnerIdentity(t *testing.T) {
	t.Parallel()
	first, second := NewResourceOwner(), NewResourceOwner()
	require.NotSame(t, first, second)
	require.Nil(t, ResourceOwnerFromContext(t.Context()))
	ctx := WithResourceOwner(t.Context(), first)
	require.Same(t, first, ResourceOwnerFromContext(ctx))
	require.Same(t, second, ResourceOwnerFromContext(WithResourceOwner(ctx, second)))
	require.Same(t, first, ResourceOwnerFromContext(ctx), "rebinding must not mutate the origin")
	require.Nil(t, ResourceOwnerFromContext(WithResourceOwner(ctx, nil)))
}
