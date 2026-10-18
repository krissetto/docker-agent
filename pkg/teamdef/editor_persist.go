package teamdef

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/config"
)

// PersistCurrent serializes persistence against edits and commits only the
// exact current editor revision. A trusted caller may provide commit to persist
// a managed immutable source; otherwise the editor's explicit local writable
// capability is used. The source is supplied only to trusted code.
func (e *Editor) PersistCurrent(ctx context.Context, base Revision, commit func(context.Context, config.Source) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current := e.source.CurrentRevision()
	if current.ID != base.ID || current.Generation != base.Generation {
		return errors.New("editable source revision conflict")
	}
	if commit == nil {
		return e.source.Persist(base)
	}
	source, err := e.source.SourceForLoad(base)
	if err != nil {
		return err
	}
	return commit(ctx, source)
}
