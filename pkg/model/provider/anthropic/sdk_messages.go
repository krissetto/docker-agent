package anthropic

import (
	"github.com/anthropics/anthropic-sdk-go/v2"
)

// SDK v2 message params are unions: a message is OfUser, OfAssistant or
// OfSystem. These helpers keep that shape in one place.

func betaUserMessage(content []anthropic.BetaContentBlockParamUnion) anthropic.BetaMessageParam {
	return anthropic.BetaMessageParam{OfUser: &anthropic.BetaUserMessageParam{Content: content}}
}

func betaAssistantMessage(content []anthropic.BetaContentBlockParamUnion) anthropic.BetaMessageParam {
	return anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{Content: content}}
}

// betaMessageContent returns the content blocks of a user or assistant message.
// System messages carry their own content type and report nil here.
func betaMessageContent(m anthropic.BetaMessageParam) []anthropic.BetaContentBlockParamUnion {
	switch {
	case m.OfUser != nil:
		return m.OfUser.Content
	case m.OfAssistant != nil:
		return m.OfAssistant.Content
	}
	return nil
}

// isBetaAssistantMessage reports whether m is an assistant message.
func isBetaAssistantMessage(m anthropic.BetaMessageParam) bool {
	return m.OfAssistant != nil
}

// isBetaSystemMessage reports whether m is a mid-conversation system message.
func isBetaSystemMessage(m anthropic.BetaMessageParam) bool {
	return m.OfSystem != nil
}

// transientSystemContent converts turn-scoped text blocks into the content of a
// mid-conversation system message.
func transientSystemContent(blocks []anthropic.BetaContentBlockParamUnion) []anthropic.BetaSystemMessageParamContentUnion {
	out := make([]anthropic.BetaSystemMessageParamContentUnion, 0, len(blocks))
	for _, b := range blocks {
		if b.OfText != nil {
			out = append(out, anthropic.BetaSystemMessageParamContentUnion{OfText: b.OfText})
		}
	}
	return out
}

// messageContent returns the content blocks of a (non-beta) user or assistant message.
func messageContent(m anthropic.MessageParam) []anthropic.ContentBlockParamUnion {
	switch {
	case m.OfUser != nil:
		return m.OfUser.Content
	case m.OfAssistant != nil:
		return m.OfAssistant.Content
	}
	return nil
}

// systemContentFromBlocks converts system-message blocks (text and tool
// addition/removal) into the content union of a mid-conversation system message.
func systemContentFromBlocks(blocks []anthropic.BetaContentBlockParamUnion) []anthropic.BetaSystemMessageParamContentUnion {
	out := make([]anthropic.BetaSystemMessageParamContentUnion, 0, len(blocks))
	for _, b := range blocks {
		switch {
		case b.OfText != nil:
			out = append(out, anthropic.BetaSystemMessageParamContentUnion{OfText: b.OfText})
		case b.OfToolAddition != nil:
			out = append(out, anthropic.BetaSystemMessageParamContentUnion{OfToolAddition: b.OfToolAddition})
		case b.OfToolRemoval != nil:
			out = append(out, anthropic.BetaSystemMessageParamContentUnion{OfToolRemoval: b.OfToolRemoval})
		}
	}
	return out
}
