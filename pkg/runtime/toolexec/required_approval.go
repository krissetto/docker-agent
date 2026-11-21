package toolexec

import "context"

type requiredApprovalKey struct{}

// WithRequiredApproval constrains this execution and its tool continuations to
// explicit per-action approval. It does not change persisted session policy or
// turn a policy/guard denial into a prompt that could override it.
func WithRequiredApproval(ctx context.Context) context.Context {
	return context.WithValue(ctx, requiredApprovalKey{}, true)
}

func requiredApproval(ctx context.Context) bool {
	v, _ := ctx.Value(requiredApprovalKey{}).(bool)
	return v
}
