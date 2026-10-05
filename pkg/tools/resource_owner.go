package tools

import "context"

// ResourceOwner is an opaque execution-resource identity. Hosts retain one
// identity per session admission, independent of externally supplied session IDs.
type ResourceOwner struct {
	_ byte
}

// NewResourceOwner creates a distinct owner for a session's resources.
func NewResourceOwner() *ResourceOwner {
	return &ResourceOwner{}
}

type resourceOwnerContextKey struct{}

// ResourceOwnerStopper releases only resources bound to the context's owner.
// Definition-level toolset shutdown must remain independent of session retirement.
type ResourceOwnerStopper interface {
	StopResourceOwner(ctx context.Context) error
}

// WithResourceOwner binds tool execution and resource cleanup to owner.
func WithResourceOwner(ctx context.Context, owner *ResourceOwner) context.Context {
	return context.WithValue(ctx, resourceOwnerContextKey{}, owner)
}

// ResourceOwnerFromContext returns nil when no host execution owner is bound.
func ResourceOwnerFromContext(ctx context.Context) *ResourceOwner {
	owner, _ := ctx.Value(resourceOwnerContextKey{}).(*ResourceOwner)
	return owner
}
