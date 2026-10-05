package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// MaxGeneratedFileBytes bounds artifact reads independently of transport framing.
const MaxGeneratedFileBytes = 32 << 20

// GeneratedFileResolver retrieves media through the canonical viewing session.
type GeneratedFileResolver interface {
	ResolveGeneratedFile(ctx context.Context, ref GeneratedFileRef) (*ResolvedGeneratedFile, error)
}

func generatedMediaParts(message *session.Message) []chat.MessagePart {
	if message == nil || message.Implicit || message.Message.Role != chat.MessageRoleAssistant {
		return nil
	}
	var parts []chat.MessagePart
	for _, part := range message.Message.MultiContent {
		if part.Type != chat.MessagePartTypeDocument || part.Document == nil {
			continue
		}
		source := part.Document.Source
		if source.ArtifactOwnerSessionID == "" || source.ArtifactPath == "" {
			continue
		}
		// Only portable metadata crosses the event boundary, never inline payloads.
		parts = append(parts, chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
			Name: part.Document.Name, MimeType: part.Document.MimeType,
			Source: chat.DocumentSource{ArtifactOwnerSessionID: source.ArtifactOwnerSessionID, ArtifactRoot: source.ArtifactRoot, ArtifactPath: source.ArtifactPath},
		}})
	}
	return parts
}

func sessionContainsGeneratedFile(sess *session.Session, ref GeneratedFileRef) bool {
	if sess == nil {
		return false
	}
	for i := range sess.Messages {
		if !sess.Messages[i].IsMessage() {
			continue
		}
		for _, part := range generatedMediaParts(sess.Messages[i].Message) {
			source := part.Document.Source
			if source.ArtifactOwnerSessionID == ref.OwnerSessionID && source.ArtifactRoot == ref.Root && source.ArtifactPath == ref.Path {
				return true
			}
		}
	}
	return false
}

func (h *sessionHandle) generatedFileVisible(ctx context.Context, ref GeneratedFileRef) bool {
	viewer, err := h.Snapshot(ctx)
	if err != nil {
		return false
	}
	if sessionContainsGeneratedFile(viewer, ref) {
		return true
	}
	if h.runtime.subagents == nil {
		return false
	}
	tree, err := (&localSessionRuntimeView{runtime: h.runtime}).InspectSessionTree(ctx, h.ID())
	if err != nil || tree == nil {
		return false
	}
	root, ok := subtreeForSession(*tree, h.ID())
	if !ok || !slices.Contains(subtreeSessionIDs(root), ref.OwnerSessionID) {
		return false
	}
	// Membership comes from the canonical tree, not the supplied owner identity.
	owner, err := h.runtime.SessionByID(ref.OwnerSessionID)
	if err != nil {
		return false
	}
	transcript, err := owner.Snapshot(ctx)
	return err == nil && sessionContainsGeneratedFile(transcript, ref)
}

func (h *sessionHandle) ResolveGeneratedFile(ctx context.Context, ref GeneratedFileRef) (*ResolvedGeneratedFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !h.generatedFileVisible(ctx, ref) {
		return nil, ErrGeneratedFileUnavailable
	}
	result, err := h.runtime.ResolveGeneratedFile(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !h.generatedFileVisible(ctx, ref) {
		return nil, ErrGeneratedFileUnavailable
	}
	return result, nil
}

func (s *remoteSession) ResolveGeneratedFile(ctx context.Context, ref GeneratedFileRef) (*ResolvedGeneratedFile, error) {
	if s.ID() == "" || s.ID() == "." || s.ID() == ".." ||
		ref.OwnerSessionID == "" || ref.Root != chat.ArtifactRootWorkspace ||
		!fs.ValidPath(ref.Path) || ref.Path == "." || strings.HasPrefix(ref.Path, "~") || strings.ContainsAny(ref.Path, "\\:\x00\r\n") {
		return nil, ErrGeneratedFileUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := json.Marshal(ref)
	if err != nil {
		return nil, err
	}
	c := s.runtime.client
	u := *c.baseURL
	u.RawPath = path.Join(u.EscapedPath(), api.SessionAPIPath, url.PathEscape(s.ID()), "generated-media")
	u.Path, err = url.PathUnescape(u.RawPath)
	if err != nil {
		return nil, err
	}
	u.RawQuery, u.Fragment = "", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	// A redirect must not move session authority or credentials to another route.
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGeneratedFileUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/octet-stream" || response.ContentLength > MaxGeneratedFileBytes {
		return nil, ErrGeneratedFileUnavailable
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, MaxGeneratedFileBytes+1))
	if err != nil || len(data) > MaxGeneratedFileBytes {
		return nil, ErrGeneratedFileUnavailable
	}
	// Never treat a server filesystem path as client filesystem authority.
	return &ResolvedGeneratedFile{Data: data, Path: ref.Path}, nil
}
