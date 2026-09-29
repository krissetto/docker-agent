// Package help defines the binding lists exposed by interactive components.
package help

import "github.com/docker/docker-agent/pkg/tui/widgets/key"

// KeyMap supplies compact and expanded help in display order.
type KeyMap interface {
	ShortHelp() []key.Binding
	FullHelp() [][]key.Binding
}
