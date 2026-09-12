package leantui

import "github.com/docker/docker-agent/pkg/leantui/ui"

func builtinCommands() []ui.Command {
	return []ui.Command{
		{Name: "new", Desc: "Start a new session", Kind: ui.CmdBuiltin},
		{Name: "sessions", Desc: "Browse/resume sessions from the current directory", Kind: ui.CmdBuiltin},
		{Name: "load", Desc: "Load a session by ID", Kind: ui.CmdBuiltin},
		{Name: "delete", Desc: "Delete a session by ID", Kind: ui.CmdBuiltin},
		{Name: "star", Desc: "Star or unstar a session", Kind: ui.CmdBuiltin},
		{Name: "compact", Desc: "Summarize and compact the conversation", Kind: ui.CmdBuiltin},
		{Name: "model", Desc: "Change the model for the current agent", Kind: ui.CmdBuiltin},
		{Name: "effort", Desc: "Set the model's reasoning effort (usage: /effort <level>)", Kind: ui.CmdBuiltin},
		{Name: "copy", Desc: "Copy the last assistant response", Kind: ui.CmdBuiltin},
		{Name: "clear", Desc: "Clear the screen", Kind: ui.CmdBuiltin},
		{Name: "help", Desc: "Show keyboard shortcuts", Kind: ui.CmdBuiltin},
		{Name: "exit", Desc: "Exit", Kind: ui.CmdBuiltin},
		{Name: "quit", Desc: "Exit", Kind: ui.CmdBuiltin},
	}
}
