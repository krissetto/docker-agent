package messages

import tea "charm.land/bubbletea/v2"

// PointerUpdateMsg contains coalesced pointer input. Wheel coordinates are the
// latest pointer position. Motion is set only when there is no wheel input.
type PointerUpdateMsg struct {
	X, Y       int
	WheelDelta int
	HasWheel   bool
	Motion     *tea.MouseMotionMsg
}

// PointerBoundaryMsg delivers pending input before a click or release.
type PointerBoundaryMsg struct {
	Pending PointerUpdateMsg
	Event   tea.Msg
}
