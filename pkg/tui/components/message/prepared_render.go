package message

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// PreparedRender contains only immutable output and the inputs which identify it.
type PreparedRender struct {
	revision          uint64
	sender            string
	msgType           types.MessageType
	sameAgent         bool
	media             []types.AssistantMedia
	content           string
	width             int
	theme, agents     uint64
	imageID           int
	selected, hovered bool
	output            string
	blocks            []markdown.CodeBlock
}

func (mv *messageModel) preparedValid(width int) bool {
	return mv.matchesPrepared(mv.prepared, width)
}

func (mv *messageModel) matchesPrepared(p *PreparedRender, width int) bool {
	return p != nil && mv.message != nil &&
		p.revision == mv.renderRevision && p.sender == mv.message.Sender &&
		p.msgType == mv.message.Type && p.sameAgent == mv.sameAgentAsPrevious(mv.message) &&
		slices.Equal(p.media, mv.message.AssistantMedia) &&
		p.width == width && p.content == mv.message.Content &&
		p.theme == styles.ThemeGeneration() && p.agents == styles.AgentColorGeneration() &&
		p.imageID == mv.markdownImageID && p.selected == mv.selected && p.hovered == mv.hovered
}

// PrepareRender captures presentation inputs on the owner; only the returned
// pure function runs on a worker. No component, theme or animation is borrowed.
func PrepareRender(view Model) func() *PreparedRender {
	mv, ok := view.(*messageModel)
	if !ok || mv.message == nil || mv.message.Type != types.MessageTypeAssistant || len(mv.message.Content) < 16*1024 || mv.preparedValid(mv.width) {
		return nil
	}
	mv.ensureTheme()
	msg := mv.message
	style := styles.AssistantMessageStyle
	if mv.selected {
		style = styles.SelectedMessageStyle
	}
	width := mv.width
	inner := width - style.GetHorizontalFrameSize()
	content, placeholders := mv.markdownImagePlaceholders(msg.Content, inner)
	// Media rendering can hold terminal image state; leave that small envelope
	// on the owner, and hand only its immutable ANSI lines to the worker.
	media := appendAssistantMediaLines("", msg.AssistantMedia, inner)
	prefix := ""
	if !mv.sameAgentAsPrevious(msg) {
		prefix = mv.senderPrefix(msg.Sender)
	}
	top := actionRow(inner, mv.hovered || mv.selected, types.MessageCopyLabel)
	renderer := markdown.NewFastRenderer(inner).FreezeStyles()
	result := PreparedRender{
		revision: mv.renderRevision, sender: msg.Sender, msgType: msg.Type,
		sameAgent: mv.sameAgentAsPrevious(msg), media: slices.Clone(msg.AssistantMedia),
		content: msg.Content, width: width,
		theme: styles.ThemeGeneration(), agents: styles.AgentColorGeneration(),
		imageID: mv.markdownImageID, selected: mv.selected, hovered: mv.hovered,
	}
	return func() *PreparedRender {
		rendered, blocks, err := renderer.RenderWithCodeBlocks(content)
		if err != nil {
			rendered = content
			blocks = nil
		}
		rendered, blocks = replaceMarkdownImagePlaceholders(rendered, blocks, placeholders)
		rendered = appendRenderedAssistantMedia(rendered, media)
		offset := strings.Count(prefix, "\n") + 1
		for i := range blocks {
			blocks[i].Line += offset
		}
		result.output = prefix + style.Width(width).Render(top+"\n"+rendered)
		result.blocks = blocks
		return &result
	}
}

func ApplyPreparedRender(view Model, prepared *PreparedRender) bool {
	mv, ok := view.(*messageModel)
	if !ok || !mv.matchesPrepared(prepared, mv.width) {
		return false
	}
	mv.prepared = prepared
	mv.codeBlocks = prepared.blocks
	return true
}

// ReleasePreparedRender drops the temporary worker artifact after the list has
// transferred its immutable output into the retained transcript. It does not
// invalidate that output or re-arm a finalized view's streaming caches.
func ReleasePreparedRender(view Model) {
	if mv, ok := view.(*messageModel); ok {
		mv.prepared = nil
	}
}
