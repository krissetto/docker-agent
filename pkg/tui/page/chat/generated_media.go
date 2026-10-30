package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/streamcontent"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Generated media rendering.
//
// The run loop materializes model-generated images into the owning
// session's workspace and persists manifest-gated references (see
// materializeGeneratedMedia in pkg/runtime/loop.go). This file renders
// those references: a sanitized "unavailable" placeholder is attached
// synchronously, and the actual bytes + validated canonical path are
// resolved through the runtime's generated-file resolver capability inside
// a tea.Cmd — never synchronously in Update — then swapped in by ID via
// messages.Model.UpdateAssistantMedia. Runtimes without the capability
// (e.g. remote) render nothing, as before.

// generatedMediaIDs issues process-unique placeholder IDs, so a resolution
// result can never match a placeholder from another page or an earlier
// session load.
var generatedMediaIDs atomic.Uint64

// generatedMediaRequest pairs one pending placeholder with the reference
// the resolver needs.
type generatedMediaRequest struct {
	id       uint64
	ref      runtime.GeneratedFileRef
	name     string
	mimeType string
}

// generatedMediaResolvedMsg delivers asynchronously resolved media items
// back to the page, which applies them by ID.
type generatedMediaResolvedMsg struct {
	media []types.AssistantMedia
}

// handleMessageAdded surfaces model-generated media in the turn it was
// produced: the run loop announces the persisted assistant message via
// MessageAddedEvent; without this handler the file sits in the workspace
// with nothing visible in the chat.
func (p *chatPage) handleMessageAdded(msg *runtime.MessageAddedEvent) tea.Cmd {
	if p.streamCancelled || msg.CommittedRole() != chat.MessageRoleAssistant {
		return nil
	}
	parts, agentName := msg.GeneratedMedia, msg.AgentName
	messageID := ""
	if msg.Message != nil {
		parts = msg.Message.Message.MultiContent
		messageID = msg.Message.Message.MessageID
		if msg.Message.AgentName != "" {
			agentName = msg.Message.AgentName
		}
	}
	identity := p.contentIdentity.Resolve(p.contentSession(msg.SessionID), messageID)
	if msg.Message != nil && msg.Message.Message.Content != "" {
		// The canonical text itself was settled by messages.CompleteAssistant.
		p.hasReceivedAssistantContent = true
		p.setPendingResponse(false)
		defer p.contentIdentity.Finish(identity.SessionID)
	}
	if !p.app.CanResolveGeneratedFiles() {
		return nil
	}
	placeholders, requests := generatedImageMedia(identity.SessionID, identity.MessageID, parts, &p.contentIdentity)
	// A legacy canonical replay adopts the identity of the restored manifest item it matches.
	p.messages.AdoptAssistantMediaIdentity(identity.SessionID, identity.MessageID, agentName, placeholders)
	if len(placeholders) == 0 {
		return nil
	}

	p.trackContentSession(msg.SessionID)
	p.hasReceivedAssistantContent = true
	p.setPendingResponse(false)
	return tea.Batch(
		p.sidebar.SetAgentActivity(agentName),
		p.messages.AppendAssistantMediaContent(identity.SessionID, identity.MessageID, agentName, placeholders),
		p.resolveGeneratedMediaCmd(requests),
	)
}

// collectRestoredGeneratedMedia extracts the generated media of every
// restored assistant message, keyed by its index in sess.Messages, for
// messages.Model.LoadFromSession, plus the resolution requests to run
// asynchronously. Nil when the runtime cannot resolve generated files.
func (p *chatPage) collectRestoredGeneratedMedia(sess *session.Session) (map[int][]types.AssistantMedia, []generatedMediaRequest) {
	if !p.app.CanResolveGeneratedFiles() {
		return nil, nil
	}
	return collectGeneratedMedia(sess, &p.contentIdentity)
}

// collectGeneratedMedia extracts restored generated media. registry is nil
// when called off the UI goroutine (background replay), in which case every
// placeholder gets a fresh ID instead of reusing one the page already issued.
func collectGeneratedMedia(sess *session.Session, registry *streamcontent.Tracker) (map[int][]types.AssistantMedia, []generatedMediaRequest) {
	var restored map[int][]types.AssistantMedia
	var requests []generatedMediaRequest
	for pos, item := range sess.Messages {
		if !item.IsMessage() || item.Message.Implicit || item.Message.Message.Role != chat.MessageRoleAssistant {
			continue
		}
		placeholders, reqs := generatedImageMedia(sess.ID, item.Message.Message.MessageID, item.Message.Message.MultiContent, registry)
		if len(placeholders) == 0 {
			continue
		}
		if restored == nil {
			restored = make(map[int][]types.AssistantMedia)
		}
		restored[pos] = placeholders
		requests = append(requests, reqs...)
	}
	return restored, requests
}

// generatedImageMedia extracts the generated images from an assistant
// message's parts: document parts carrying an owner-qualified generated-file
// reference and an image MIME type. Every extracted item starts as a
// sanitized "unavailable" placeholder; items whose root kind the resolver
// supports additionally get a resolution request. References with an
// unknown (empty) root kind stay unavailable by design. User attachments
// (inline sources) and ownerless references are not extracted.
//
// Each item gets a stable Key derived from session, message and artifact. When
// registry is non-nil, a key seen before reuses its placeholder ID and is not
// requested again, so a replayed canonical reference joins the same item.
func generatedImageMedia(sessionID, messageID string, parts []chat.MessagePart, registry *streamcontent.Tracker) ([]types.AssistantMedia, []generatedMediaRequest) {
	var media []types.AssistantMedia
	var requests []generatedMediaRequest
	for _, part := range parts {
		doc := part.Document
		if part.Type != chat.MessagePartTypeDocument || doc == nil {
			continue
		}
		src := doc.Source
		if src.ArtifactPath == "" || src.ArtifactOwnerSessionID == "" {
			continue
		}
		if !chat.IsImageMimeType(doc.MimeType) {
			continue
		}

		name := chat.SanitizeDisplayName(doc.Name)
		if name == "" {
			name = "generated media"
		}
		// A legacy (ID-less) stream key ignores the message ID, so the
		// restored manifest entry and its later canonical replay still match.
		keyMessageID := messageID
		if streamcontent.IsLegacy(messageID) {
			keyMessageID = ""
		}
		key := fmt.Sprintf("%q:%q:%q:%q:%q:%q:%q", sessionID, keyMessageID, src.ArtifactOwnerSessionID, src.ArtifactRoot, src.ArtifactPath, doc.MimeType, name)
		id, seen := uint64(0), false
		if registry != nil {
			id, seen = registry.MediaID(key)
		}
		if !seen {
			if src.ArtifactRoot == chat.ArtifactRootWorkspace {
				id = generatedMediaIDs.Add(1)
			}
			if registry != nil {
				registry.RememberMedia(key, id)
			}
		}
		item := types.AssistantMedia{ID: id, Key: key, Fallback: fmt.Sprintf("Generated image %q is unavailable.", name)}
		if src.ArtifactRoot == chat.ArtifactRootWorkspace && !seen {
			requests = append(requests, generatedMediaRequest{
				id: item.ID,
				ref: runtime.GeneratedFileRef{
					OwnerSessionID: src.ArtifactOwnerSessionID,
					Root:           src.ArtifactRoot,
					Path:           src.ArtifactPath,
				},
				name:     name,
				mimeType: doc.MimeType,
			})
		}
		media = append(media, item)
	}
	return media, requests
}

// resolveGeneratedMediaCmd resolves the requested items on a background
// goroutine and routes the results back to this page (its tab may be
// hidden — or another tab active — by the time they arrive). The command
// is also recorded like a routed timer so an update on a hidden tab keeps
// the resolution armed.
func (p *chatPage) resolveGeneratedMediaCmd(requests []generatedMediaRequest) tea.Cmd {
	if len(requests) == 0 {
		return nil
	}
	application, ctx, routingID := p.app, p.ctx(), p.routingID
	cmd := func() tea.Msg {
		media := make([]types.AssistantMedia, 0, len(requests))
		for _, req := range requests {
			media = append(media, resolveGeneratedImage(ctx, application, req))
		}
		var inner tea.Msg = generatedMediaResolvedMsg{media: media}
		if routingID == "" {
			return inner
		}
		return msgtypes.RoutedMsg{SessionID: routingID, Inner: inner}
	}
	p.pendingTimers = append(p.pendingTimers, cmd)
	return cmd
}

// resolveGeneratedImage resolves one generated image through the runtime's
// manifest-gated resolver and prepares it for terminal rendering. Every
// returned item carries a Fallback built exclusively from safe display
// data — the sanitized name and, when resolution validated it, the
// canonical workspace path. Raw resolver errors, references, and owner
// session IDs never reach the fallback; they go to the debug log only.
func resolveGeneratedImage(ctx context.Context, application *app.App, req generatedMediaRequest) types.AssistantMedia {
	fallback := fmt.Sprintf("Generated image %q is unavailable.", req.name)
	resolved, err := application.ResolveGeneratedFile(ctx, req.ref)
	if err != nil {
		slog.DebugContext(ctx, "Generated file could not be resolved for display", "name", req.name, "error", err)
		return types.AssistantMedia{ID: req.id, Fallback: fallback}
	}
	if resolved.Path != "" && displaySafePath(resolved.Path) {
		fallback = fmt.Sprintf("Generated image %q saved to: %s", req.name, resolved.Path)
	}
	inline, decoded := tuiimage.FromBytes(req.name, req.mimeType, resolved.Data)
	if !decoded {
		slog.DebugContext(ctx, "Generated file could not be decoded for display", "name", req.name)
		return types.AssistantMedia{ID: req.id, Fallback: fallback}
	}
	return types.AssistantMedia{ID: req.id, Image: &inline, Fallback: fallback}
}

// displaySafePath reports whether path can be shown verbatim in the chat:
// the canonical path comes from our own validated resolver, but a filename
// containing control characters must still never reach the terminal.
func displaySafePath(path string) bool {
	return !strings.ContainsFunc(path, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	})
}
