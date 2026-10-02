package messages

// OpenWorkingDirMsg opens a local directory with the platform handler.
type OpenWorkingDirMsg struct{ Path string }

// ShowInteractionHintMsg displays short feedback owned by the selected session.
type ShowInteractionHintMsg struct {
	SessionID string
	Text      string
}

// OpenPendingEditMsg opens a draft for one accepted input without changing its identity.
type OpenPendingEditMsg struct{ SessionID, TurnID, Content string }

// OpenPendingRemovalMsg requests confirmation for one canonical pending turn.
type OpenPendingRemovalMsg struct{ SessionID, TurnID, Content string }
