package runtime

import (
	"context"
	"errors"
	"sync"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/teamdef"
)

// TeamEditPersistFunc is injected by a trusted host for managed sources. The
// expected source revision is an opaque CAS token; candidate is an immutable
// in-memory authored source and must not be exposed to user-controlled paths.
type TeamEditPersistFunc func(context.Context, string, config.Source) (string, error)

// TeamEditPersistence carries trusted source-commit authority. It is never
// populated from team YAML or tool arguments.
type TeamEditPersistence struct {
	ExpectedSourceRevision string
	Persist                TeamEditPersistFunc
	mu                     sync.Mutex
}

type teamEditPersistenceReceipt struct {
	Revision       string `json:"revision"`
	SourceRevision string `json:"source_revision,omitempty"`
	Persisted      bool   `json:"persisted"`
}

type teamEditExportReceipt struct {
	Revision string `json:"revision"`
	Exported bool   `json:"exported"`
}

func persistTeamEditSource(ctx context.Context, editor *teamdef.Editor, base teamdef.Revision, persistence *TeamEditPersistence) (teamEditPersistenceReceipt, error) {
	if editor == nil {
		return teamEditPersistenceReceipt{}, errors.New("team editor is unavailable")
	}
	if persistence == nil || persistence.Persist == nil {
		if err := editor.PersistCurrent(ctx, base, nil); err != nil {
			return teamEditPersistenceReceipt{}, err
		}
		return teamEditPersistenceReceipt{Revision: base.ID, Persisted: true}, nil
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if persistence.ExpectedSourceRevision == "" {
		return teamEditPersistenceReceipt{}, errors.New("managed source revision is unavailable")
	}
	expected := persistence.ExpectedSourceRevision
	var next string
	err := editor.PersistCurrent(ctx, base, func(ctx context.Context, candidate config.Source) error {
		var err error
		next, err = persistence.Persist(ctx, expected, candidate)
		if err != nil {
			return err
		}
		if next == "" {
			return errors.New("managed source commit returned no revision")
		}
		return nil
	})
	if err != nil {
		return teamEditPersistenceReceipt{}, err
	}
	if next == "" {
		return teamEditPersistenceReceipt{}, errors.New("managed source commit returned no revision")
	}
	persistence.ExpectedSourceRevision = next
	return teamEditPersistenceReceipt{Revision: base.ID, SourceRevision: next, Persisted: true}, nil
}

func exportTeamEditSource(editor *teamdef.Editor, revision teamdef.Revision, path string) (teamEditExportReceipt, error) {
	if editor == nil {
		return teamEditExportReceipt{}, errors.New("team editor is unavailable")
	}
	if err := editor.Source().ExportNew(revision, path); err != nil {
		return teamEditExportReceipt{}, err
	}
	return teamEditExportReceipt{Revision: revision.ID, Exported: true}, nil
}
