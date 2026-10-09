package tools

import (
	"context"
	"errors"
	"reflect"
	"sync"
)

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

type resourceCleanupIdentity struct {
	typ reflect.Type
	ptr uintptr
}

type resourceCleanupSeen struct {
	mu         sync.Mutex
	identities map[resourceCleanupIdentity]struct{}
}

type resourceCleanupSeenKey struct{}

// StopResourceOwners releases an owner's resources across wrappers and composites.
// Pointer aliases are visited once; value implementations have no safe identity
// and are visited at each occurrence. Composite stoppers propagate ctx unchanged.
func StopResourceOwners(ctx context.Context, toolsets ...ToolSet) error {
	seen, ok := ctx.Value(resourceCleanupSeenKey{}).(*resourceCleanupSeen)
	if !ok {
		seen = &resourceCleanupSeen{identities: make(map[resourceCleanupIdentity]struct{})}
		ctx = context.WithValue(ctx, resourceCleanupSeenKey{}, seen)
	}
	var errs []error
	for _, ts := range toolsets {
		stopper, ok := As[ResourceOwnerStopper](ts)
		if !ok {
			continue
		}
		v := reflect.ValueOf(stopper)
		if v.Kind() == reflect.Pointer {
			key := resourceCleanupIdentity{typ: v.Type(), ptr: v.Pointer()}
			seen.mu.Lock()
			_, dup := seen.identities[key]
			seen.identities[key] = struct{}{}
			seen.mu.Unlock()
			if dup {
				continue
			}
		}
		errs = append(errs, stopper.StopResourceOwner(ctx))
	}
	return errors.Join(errs...)
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
