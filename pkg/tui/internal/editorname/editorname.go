// Package editorname resolves the user's configured external editor (VISUAL
// or EDITOR): a friendly display name used in TUI key-binding hints, and the
// command that launches the editor over a file.
//
// The environment-reading entry points are thin wrappers over pure functions
// (FromEnv, CommandFromEnv) that take the raw variable values as parameters,
// so every code path is testable across platforms and editor configurations
// without touching the actual process environment.
package editorname

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// editorPrefixes maps a binary-name prefix (e.g. "code") to a friendly display
// name (e.g. "VSCode"). Order matters: longer / more specific prefixes must
// appear before shorter ones (e.g. "vim" before "vi").
var editorPrefixes = []struct {
	prefix string
	name   string
}{
	{"code", "VSCode"},
	{"cursor", "Cursor"},
	{"nvim", "Neovim"},
	{"vim", "Vim"},
	{"vi", "Vi"},
	{"nano", "Nano"},
	{"emacs", "Emacs"},
	{"subl", "Sublime Text"},
	{"sublime", "Sublime Text"},
	{"atom", "Atom"},
	{"gedit", "gedit"},
	{"kate", "Kate"},
	{"notepad++", "Notepad++"},
	{"notepad", "Notepad"},
	{"textmate", "TextMate"},
	{"mate", "TextMate"},
	{"zed", "Zed"},
}

// FromEnv returns a friendly display name for the configured external editor.
// It reads VISUAL first, then falls back to EDITOR. When neither is set, it
// returns the platform-specific fallback ("Notepad" on Windows, "Vi" elsewhere)
// that matches the actual command that will be launched.
//
// FromEnv is pure: it takes the raw environment values as parameters so that
// tests can exercise every code path without mutating os.Environ.
func FromEnv(visual, editorEnv string) string {
	if visual == "" && editorEnv == "" {
		if goruntime.GOOS == "windows" {
			return "Notepad"
		}
		return "Vi"
	}
	editorCmd := strings.TrimSpace(visual)
	if editorCmd == "" {
		editorCmd = strings.TrimSpace(editorEnv)
	}
	if editorCmd == "" {
		return "$EDITOR"
	}

	parts, err := splitCommand(editorCmd)
	if err != nil || len(parts) == 0 {
		return "$EDITOR"
	}

	baseName := filepath.Base(parts[0])

	for _, e := range editorPrefixes {
		if strings.HasPrefix(baseName, e.prefix) {
			return e.name
		}
	}

	if baseName != "" {
		// Capitalize the first rune (not byte) so that names beginning with
		// multi-byte UTF-8 characters survive the round-trip.
		r, size := utf8.DecodeRuneInString(baseName)
		if r != utf8.RuneError {
			return string(unicode.ToUpper(r)) + baseName[size:]
		}
	}

	return "$EDITOR"
}

// Command builds an editor command using VISUAL, EDITOR, or the platform default.
// Invalid or unavailable configured editors produce a command that fails with
// a useful error when started; callers that need synchronous validation should
// use PrepareCommandFromEnv.
func Command(path string) *exec.Cmd {
	return CommandFromEnv(os.Getenv("VISUAL"), os.Getenv("EDITOR"), path)
}

// CommandFromEnv preserves the existing convenience API while using safe
// argument parsing. Call PrepareCommandFromEnv when launch errors should be
// reported before scheduling the process.
func CommandFromEnv(visual, editorEnv, path string) *exec.Cmd {
	cmd, err := buildCommand(visual, editorEnv, path)
	if err != nil {
		return failedCommand(err)
	}
	return cmd
}

// PrepareCommandFromEnv constructs an argv-based editor command without a
// shell. Quotes and backslash escapes in VISUAL/EDITOR only group arguments;
// environment expansion, command substitution, and shell operators are not
// evaluated. VISUAL takes precedence when it contains a command, then EDITOR.
func PrepareCommandFromEnv(visual, editorEnv, path string) (*exec.Cmd, error) {
	cmd, err := buildCommand(visual, editorEnv, path)
	if err != nil {
		return nil, err
	}
	if _, err := exec.LookPath(cmd.Args[0]); err != nil {
		return nil, fmt.Errorf("external editor %q is unavailable: %w", cmd.Args[0], err)
	}
	return cmd, nil
}

func buildCommand(visual, editorEnv, path string) (*exec.Cmd, error) {
	configured := strings.TrimSpace(visual)
	if configured == "" {
		configured = strings.TrimSpace(editorEnv)
	}
	var parts []string
	if configured != "" {
		parsed, err := splitCommand(configured)
		if err != nil {
			return nil, fmt.Errorf("parse external editor command: %w", err)
		}
		parts = parsed
	}
	if len(parts) == 0 {
		if goruntime.GOOS == "windows" {
			parts = []string{"notepad"}
		} else {
			parts = []string{"vi"}
		}
	}
	args := append(append([]string(nil), parts[1:]...), path)
	cmd := exec.Command(parts[0], args...) //nolint:noctx // owned by tea.ExecProcess
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd, nil
}

func failedCommand(err error) *exec.Cmd {
	return &exec.Cmd{Err: err, Args: []string{""}}
}

func splitCommand(command string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	runes := []rune(command)
	escaped, started := false, false
	for i, r := range runes {
		if escaped {
			word.WriteRune(r)
			escaped, started = false, true
			continue
		}
		if r == '\\' && quote != '\'' {
			nextIsWhitespace := i+1 < len(runes) && strings.ContainsRune(" \t\r\n", runes[i+1])
			if quote == '"' {
				if i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '\\') {
					escaped, started = true, true
					continue
				}
			} else if i+1 < len(runes) && (nextIsWhitespace || runes[i+1] == '\'' || runes[i+1] == '"' || runes[i+1] == '\\') {
				escaped, started = true, true
				continue
			}
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote, started = r, true
			continue
		}
		if strings.ContainsRune(" \t\r\n", r) {
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if escaped || quote != 0 {
		return nil, errors.New("unterminated quote or escape")
	}
	if started {
		args = append(args, word.String())
	}
	return args, nil
}
