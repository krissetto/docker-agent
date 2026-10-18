package messages

// OpenGitReviewMsg opens read-only review for the focused workspace; Files
// starts in the workspace file browser instead of Changes.
type OpenGitReviewMsg struct{ Files bool }

// RevealGitChangesMsg opens and focuses the sidebar's Git changes section.
type RevealGitChangesMsg struct{}
