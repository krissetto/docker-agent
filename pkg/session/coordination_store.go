package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/subagent"
)

var ErrRevisionConflict = errors.New("child revision conflict")

type ChildRecord struct {
	RootSessionID   string
	ParentSessionID string
	Node            subagent.Node
	Revision        uint64
	LastTurnID      string
	Result          string
	Error           string
}

type ChildAdmission struct {
	Child  *Session
	Record ChildRecord
}

// ChildAdmissionBatchStore atomically adopts an existing, bounded legacy tree.
// Every child, parent and root row must still exist at admission; unlike
// AdmitChild, this capability never creates a missing session row.
// Runtime source, membership and grant validation must precede admission.
type ChildAdmissionBatchStore interface {
	AdmitChildren(ctx context.Context, admissions []ChildAdmission) error
}

type ChildReport struct {
	ID              string
	ParentSessionID string
	ChildSessionID  string
	TurnID          string
	Content         string
	Revision        uint64
}

type ChildCommit struct {
	ExpectedRevision uint64
	Record           ChildRecord
	Reports          []ChildReport
}

// ReportAcceptance identifies the original input even after its removal.
// Message is an independent snapshot of the current stored input, when present.
type ReportAcceptance struct {
	MessageID int64
	Message   *Message
	Created   bool
}

// CoordinationStore is optional. Durability must be advertised separately;
// implementing this capability alone does not promise persistence across restart.
// Transcript writes finish before CommitChild; revisions protect only child state.
type CoordinationStore interface {
	AdmitChild(ctx context.Context, admission ChildAdmission) error
	// A repeated successful commit returns ErrRevisionConflict. Reload records
	// and pending reports to reconcile an uncertain commit; never mint new IDs.
	CommitChild(ctx context.Context, commit ChildCommit) error
	LoadChildren(ctx context.Context, rootSessionID string) ([]ChildRecord, error)
	PendingReports(ctx context.Context, parentSessionID string) ([]ChildReport, error)
	// AcceptReport durably queues input and acknowledges the report in one step.
	// Retries return its original identity and current message, never retry content.
	// A nil message is a read-only acknowledgement probe: an unacknowledged report
	// returns a zero result; an acknowledged but removed input has a nil Message.
	// Missing reports and wrong parents return ErrNotFound in either mode.
	AcceptReport(ctx context.Context, parentSessionID, reportID string, message *Message) (ReportAcceptance, error)
}

func prepareAdmission(a ChildAdmission) (*Session, ChildRecord, error) {
	r := a.Record
	if a.Child == nil || a.Child.ID == "" || r.RootSessionID == "" || r.ParentSessionID == "" || r.Node.ID == "" {
		return nil, r, ErrEmptyID
	}
	if a.Child.ID != r.Node.SessionID || a.Child.ID == r.ParentSessionID || a.Child.ID == r.RootSessionID {
		return nil, r, errors.New("invalid child identity")
	}
	if a.Child.ParentID != "" && a.Child.ParentID != r.ParentSessionID {
		return nil, r, ErrAlreadyExists
	}
	child := a.Child.OwnSnapshot()
	child.ParentID = r.ParentSessionID
	r.Revision = 1
	return child, r, nil
}

func prepareCommit(c ChildCommit, old ChildRecord) (ChildRecord, error) {
	r := c.Record
	if c.ExpectedRevision != old.Revision || old.Revision == ^uint64(0) {
		return r, ErrRevisionConflict
	}
	if r.RootSessionID != old.RootSessionID || r.ParentSessionID != old.ParentSessionID || r.Node.ID != old.Node.ID || r.Node.SessionID != old.Node.SessionID {
		return r, errors.New("child identity cannot change")
	}
	r.Revision = old.Revision + 1
	ids := make(map[string]bool, len(c.Reports))
	for _, report := range c.Reports {
		if report.ID == "" || report.TurnID == "" || report.ChildSessionID != r.Node.SessionID || report.ParentSessionID != r.ParentSessionID || ids[report.ID] {
			return r, errors.New("invalid child report")
		}
		ids[report.ID] = true
	}
	return r, nil
}

type acceptedChildReport struct {
	report    ChildReport
	messageID int64
}

func (s *InMemorySessionStore) Durability() subagent.Durability { return subagent.DurabilityVolatile }

// AdmitChild shares the batch implementation for spawn admission, but may create
// its new child. Restore batches must not recreate rows deleted after prepare.
func (s *InMemorySessionStore) AdmitChild(ctx context.Context, a ChildAdmission) error {
	return s.admitChildren(ctx, []ChildAdmission{a}, true)
}

func (s *InMemorySessionStore) AdmitChildren(ctx context.Context, admissions []ChildAdmission) error {
	return s.admitChildren(ctx, admissions, false)
}

type preparedChildAdmission struct {
	child        *Session
	record       ChildRecord
	recordJSON   string
	createChild  bool
	recordExists bool
}

func prepareAdmissions(admissions []ChildAdmission) ([]preparedChildAdmission, error) {
	prepared := make([]preparedChildAdmission, 0, len(admissions))
	sessions := make(map[string]bool, len(admissions))
	nodes := make(map[subagent.NodeID]bool, len(admissions))
	for _, admission := range admissions {
		child, record, err := prepareAdmission(admission)
		if err != nil {
			return nil, err
		}
		if sessions[child.ID] || nodes[record.Node.ID] {
			return nil, ErrAlreadyExists
		}
		data, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		sessions[child.ID], nodes[record.Node.ID] = true, true
		prepared = append(prepared, preparedChildAdmission{child: child, record: record, recordJSON: string(data)})
	}
	return prepared, nil
}

// Lifecycle state may have advanced since admission. Idempotence compares the
// immutable ownership identity only, and never rewrites the current record.
func sameChildIdentity(old, record ChildRecord) bool {
	return old.RootSessionID == record.RootSessionID && old.ParentSessionID == record.ParentSessionID &&
		old.Node.SessionID == record.Node.SessionID && old.Node.ID == record.Node.ID &&
		old.Node.Agent == record.Node.Agent && old.Node.Parent == record.Node.Parent
}

func (s *InMemorySessionStore) admitChildren(ctx context.Context, admissions []ChildAdmission, allowCreate bool) error {
	if len(admissions) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	prepared, err := prepareAdmissions(admissions)
	if err != nil {
		return err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	lookup := func(id string) (*Session, error) {
		sess, ok := s.sessions.Load(id)
		if !ok {
			return nil, ErrNotFound
		}
		return sess, nil
	}
	for i := range prepared {
		entry := &prepared[i]
		child, record := entry.child, entry.record
		existing, err := lookup(child.ID)
		if errors.Is(err, ErrNotFound) && allowCreate {
			if err := validateNewChild(child); err != nil {
				return err
			}
			entry.createChild = true
			existing = child
		} else if err != nil {
			return err
		}
		if err := validateChildAdoption(existing, child, record, lookup); err != nil {
			return err
		}
		if old, ok := s.children[child.ID]; ok {
			if !sameChildIdentity(old, record) || entry.createChild {
				return ErrAlreadyExists
			}
			entry.recordExists = true
		}
		for id, old := range s.children {
			if id != child.ID && old.RootSessionID == record.RootSessionID && old.Node.ID == record.Node.ID {
				return ErrAlreadyExists
			}
		}
	}
	// DeleteSession uses this same mutex. After the final cancellation check,
	// applying this completely validated batch has no fallible steps.
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.children == nil {
		s.children = make(map[string]ChildRecord)
	}
	for _, entry := range prepared {
		child, record := entry.child, entry.record
		if entry.createChild {
			for _, item := range child.Messages {
				if item.Message != nil {
					item.Message.ID = s.messageID.Add(1)
				}
			}
			s.sessions.Store(child.ID, child)
		}
		parent, _ := s.sessions.Load(record.ParentSessionID)
		linked := false
		for _, item := range parent.MessagesSnapshot() {
			if item.SubSession != nil && item.SubSession.ID == child.ID {
				linked = true
				break
			}
		}
		if !linked {
			parent.AddSubSession(&Session{ID: child.ID, ParentID: record.ParentSessionID})
		}
		if !entry.recordExists {
			s.children[child.ID] = record
		}
	}
	return nil
}

func (s *InMemorySessionStore) CommitChild(ctx context.Context, c ChildCommit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	old, ok := s.children[c.Record.Node.SessionID]
	if !ok {
		return ErrNotFound
	}
	record, err := prepareCommit(c, old)
	if err != nil {
		return err
	}
	for _, report := range c.Reports {
		if _, ok := s.reports[report.ID]; ok {
			return ErrAlreadyExists
		}
	}
	if s.reports == nil {
		s.reports = make(map[string]acceptedChildReport)
	}
	for _, report := range c.Reports {
		report.Revision = record.Revision
		s.reports[report.ID] = acceptedChildReport{report: report}
	}
	s.children[record.Node.SessionID] = record
	return nil
}

func (s *InMemorySessionStore) LoadChildren(ctx context.Context, root string) ([]ChildRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	var records []ChildRecord
	for _, record := range s.children {
		if record.RootSessionID == root {
			records = append(records, record)
		}
	}
	slices.SortFunc(records, func(a, b ChildRecord) int { return a.Node.CreatedAt.Compare(b.Node.CreatedAt) })
	return records, nil
}

func (s *InMemorySessionStore) PendingReports(ctx context.Context, parent string) ([]ChildReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	var reports []ChildReport
	for _, entry := range s.reports {
		if entry.report.ParentSessionID == parent && entry.messageID == 0 {
			reports = append(reports, entry.report)
		}
	}
	slices.SortFunc(reports, func(a, b ChildReport) int {
		if a.Revision < b.Revision {
			return -1
		}
		if a.Revision > b.Revision {
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return reports, nil
}

func (s *InMemorySessionStore) AcceptReport(ctx context.Context, parent, id string, message *Message) (ReportAcceptance, error) {
	if err := ctx.Err(); err != nil {
		return ReportAcceptance{}, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	entry, ok := s.reports[id]
	if !ok || entry.report.ParentSessionID != parent {
		return ReportAcceptance{}, ErrNotFound
	}
	if entry.messageID != 0 {
		result := ReportAcceptance{MessageID: entry.messageID}
		if stored, ok := s.sessions.Load(parent); ok {
			for _, item := range stored.MessagesSnapshot() {
				if item.Message != nil && item.Message.ID == entry.messageID {
					result.Message = item.Message
					break
				}
			}
		}
		return result, nil
	}
	if message == nil {
		return ReportAcceptance{}, nil
	}
	if message.TurnID == "" {
		return ReportAcceptance{}, errors.New("report input requires a turn ID")
	}
	queued := cloneMessage(message)
	queued.Pending, queued.Accepted = true, true
	messageID, err := s.addMessage(parent, queued)
	if err != nil {
		return ReportAcceptance{}, err
	}
	entry.messageID, entry.report.Content = messageID, ""
	s.reports[id] = entry
	queued.ID = messageID
	if stored, ok := s.sessions.Load(parent); ok {
		for _, item := range stored.MessagesSnapshot() {
			if item.Message != nil && item.Message.ID == messageID {
				queued = item.Message
				break
			}
		}
	}
	return ReportAcceptance{MessageID: messageID, Message: queued, Created: true}, nil
}

var (
	_ CoordinationStore        = (*InMemorySessionStore)(nil)
	_ CoordinationStore        = (*SQLiteSessionStore)(nil)
	_ ChildAdmissionBatchStore = (*InMemorySessionStore)(nil)
	_ ChildAdmissionBatchStore = (*SQLiteSessionStore)(nil)
)

// Resolve references from the current owner row, never from an ancestor's copy.
func (s *InMemorySessionStore) materializeSession(stored *Session) *Session {
	snapshot := stored.OwnSnapshot()
	for i, item := range snapshot.Messages {
		if item.SubSession == nil {
			continue
		}
		if child, ok := s.sessions.Load(item.SubSession.ID); ok {
			snapshot.Messages[i].SubSession = s.materializeSession(child)
		} else {
			// AddSession also accepts a complete imported history tree.
			for _, original := range stored.MessagesSnapshot() {
				if original.SubSession != nil && original.SubSession.ID == item.SubSession.ID {
					snapshot.Messages[i].SubSession = original.SubSession.Clone()
					break
				}
			}
		}
	}
	return snapshot
}

// Legacy rows are adopted by reference: their current transcript and settings
// remain authoritative, even when the migration caller carries a stale snapshot.
func validateChildAdoption(existing, incoming *Session, record ChildRecord, lookup func(string) (*Session, error)) error {
	if existing.ID != incoming.ID || existing.ParentID != record.ParentSessionID || existing.Origin != incoming.Origin {
		return ErrAlreadyExists
	}
	if binding := existing.AttributesSnapshot()["docker-agent.actor.agent"]; binding != "" && binding != record.Node.Agent {
		return ErrAlreadyExists
	}
	seen := map[string]bool{existing.ID: true}
	for id := record.ParentSessionID; ; {
		if seen[id] {
			return ErrAlreadyExists
		}
		seen[id] = true
		parent, err := lookup(id)
		if err != nil {
			return err
		}
		if id == record.RootSessionID {
			if parent.ParentID != "" {
				return ErrAlreadyExists
			}
			return nil
		}
		if parent.ParentID == "" {
			return ErrAlreadyExists
		}
		id = parent.ParentID
	}
}

func validateNewChild(child *Session) error {
	for _, item := range child.Messages {
		if item.SubSession != nil {
			return errors.New("new child cannot contain descendant references")
		}
	}
	return nil
}
