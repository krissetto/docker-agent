package subagenttool

import (
	"encoding/json"
	"strings"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// spawnAgent scans only new argument bytes while pending. Task bodies are never
// decoded or copied just to draw a one-line header. The canonical arguments stay
// on the message; replacement/final arguments always supersede the preview.
// This is a cosmetic preview of completed top-level string fields, not a JSON
// validator. It tolerates unfinished/malformed pending input; only encoding/json
// interprets authoritative non-pending arguments. Prefix validation still reads
// the old prefix to detect replacement, but never decodes/copies the task body.
type spawnAgent struct {
	args                                     string
	name                                     string
	depth                                    int
	inString, escaped, keyString, agentValue bool
	start                                    int
	key                                      string
	expectingKey                             bool
	status                                   types.ToolStatus
}

func (p *spawnAgent) update(args string, status types.ToolStatus) string {
	if args == p.args && status == p.status {
		return p.name
	}
	if status != types.ToolStatusPending {
		p.args, p.status = args, status
		p.name = ""
		var parsed subagent.SpawnArgs
		if json.Unmarshal([]byte(args), &parsed) == nil {
			p.name = parsed.Agent
		}
		return p.name
	}
	if p.status != types.ToolStatusPending || !strings.HasPrefix(args, p.args) {
		*p = spawnAgent{}
	}
	offset := len(p.args)
	for i := offset; i < len(args); i++ {
		c := args[i]
		if p.inString {
			if p.escaped {
				p.escaped = false
				continue
			}
			if c == '\\' {
				p.escaped = true
				continue
			}
			if c != '"' {
				continue
			}
			p.inString = false
			if p.depth == 1 {
				if p.keyString {
					p.key = ""
					_ = json.Unmarshal([]byte(args[p.start:i+1]), &p.key)
					p.expectingKey = false
				} else if p.agentValue {
					var name string
					if json.Unmarshal([]byte(args[p.start:i+1]), &name) == nil {
						p.name = name
					}
				}
			}
			continue
		}
		switch c {
		case '{', '[':
			p.depth++
			if p.depth == 1 {
				p.expectingKey = true
			}
		case '}', ']':
			p.depth--
		case ',':
			if p.depth == 1 {
				p.expectingKey = true
				p.key = ""
			}
		case '"':
			p.inString = true
			p.start = i
			p.keyString = p.depth == 1 && p.expectingKey
			p.agentValue = p.depth == 1 && !p.expectingKey && p.key == "agent"
		}
	}
	p.args, p.status = args, status
	return p.name
}

type spawnHeaderKey struct {
	name, id, icon string
	ref            lifecycle.InputReference
	status         types.ToolStatus
	width          int
	theme, agents  uint64
}

// The retained header is tiny and excludes task arguments. Spinner phase and
// live attribution participate in the key on every call, including nested
// reasoning views, so unrelated tool deltas reuse it without freezing animation.
func newSpawnRenderer() renderFunc {
	var agent spawnAgent
	var key spawnHeaderKey
	var output string
	return func(msg *types.Message, s spinner.Spinner, _ service.SessionStateReader, width, _ int, lookup NameLookup) string {
		name, id := attribution(msg, "", lookup)
		if name == "" {
			name = agent.update(msg.ToolCall.Function.Arguments, msg.ToolStatus)
		}
		next := spawnHeaderKey{name: name, id: id, icon: statusIcon(msg, s), ref: msg.InputReference, status: msg.ToolStatus, width: width, theme: styles.ThemeGeneration(), agents: styles.AgentColorGeneration()}
		if next != key || output == "" {
			output = line(msg, s, verb(msg, "Spawning", "Spawned"), name, id, width)
			key = next
		}
		return output
	}
}
