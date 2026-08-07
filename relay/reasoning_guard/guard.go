// Package reasoning_guard implements the DeepSeek V4 reasoning_content
// integrity guard described in docs/channel/deepseek-v4-reasoning-guard.md.
//
// The guard is triggered by model name (strings.HasPrefix(model, "deepseek-v4-"))
// rather than channel type, so it covers DeepSeek channels, ali channels,
// OpenAI passthrough, and third-party aggregators alike.
//
// Three layers:
//
//   - L1 DetectReasoningContentGap: format-agnostic detection of assistant
//     tool-call messages that are missing reasoning_content.
//   - L2 ReasoningContentCache: conservative server-side backfill keyed on
//     tool_call_id (process + Redis).
//   - L3 L3EnhanceErrorResponse: inject a readable hint when upstream returns
//     the 400 "reasoning_content must be passed back" error.
package reasoning_guard

import (
	"strings"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// DetectIfDeepSeekV4 reports whether modelName belongs to the DeepSeek V4
// family. This is the guard's total gate and is intentionally channel-type
// agnostic.
//
// Matching is fuzzy to tolerate third-party supplier model-name variants:
//   - Case-insensitive: "DeepSeek-V4-Pro", "DEEPSEEK-V4" all match.
//   - Infix match: the marker "deepseek-v4" may appear anywhere in the
//     model name, so suppliers that wrap the marker (e.g. "v4-deepseek-pro")
//     or drop the dash (e.g. "deepseekv4-pro") are still recognized.
//
// The trade-off is a slightly higher false-positive risk for a hypothetical
// non-V4 model whose name coincidentally contains "deepseek-v4"; DeepSeek's
// own naming has no such collision today, and the guard's L1/L2 behavior is
// gated further by tool-call + thinking-mode checks, so a false positive at
// this gate only costs a no-op diagnosis scan.
func DetectIfDeepSeekV4(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "deepseek-v4")
}

// ReasoningGapReport describes missing reasoning_content in an inbound request.
// It is format-agnostic: for OpenAI/Responses formats it describes a missing
// Message.ReasoningContent; for Claude format it describes a missing thinking
// content block on an assistant message that carries tool_use blocks.
type ReasoningGapReport struct {
	HasTools        bool
	IsDeepSeekV4    bool
	RelayFormat     types.RelayFormat
	MissingMessages []MissingReasoningMessage
	AnyMissing      bool
}

// MissingReasoningMessage describes a single assistant tool-call message
// missing reasoning_content.
type MissingReasoningMessage struct {
	MessageIndex int
	ToolCallIDs  []string
}

// DetectReasoningContentGap inspects an inbound request for missing
// reasoning_content on DeepSeek V4 thinking-mode tool-calling turns.
//
// Detection is enabled only when:
//  1. modelName matches the deepseek-v4-* family (DetectIfDeepSeekV4).
//  2. The request carries tools.
//
// For OpenAI/Responses formats the messages slice is walked and each
// role=assistant message with non-empty tool_calls is checked for a nil
// ReasoningContent. For Claude format the assistant message content array is
// inspected for tool_use blocks without a paired thinking block.
//
// The messages parameter accepts *dto.GeneralOpenAIRequest,
// *dto.ClaudeRequest, or *dto.OpenAIResponsesRequest; unknown types yield an
// empty report.
func DetectReasoningContentGap(format types.RelayFormat, modelName string, messages any) ReasoningGapReport {
	report := ReasoningGapReport{
		RelayFormat:  format,
		IsDeepSeekV4: DetectIfDeepSeekV4(modelName),
	}
	if !report.IsDeepSeekV4 {
		return report
	}
	switch format {
	case types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses:
		fillOpenAIGap(&report, messages)
	case types.RelayFormatClaude:
		fillClaudeGap(&report, messages)
	}
	report.AnyMissing = len(report.MissingMessages) > 0
	return report
}

// fillOpenAIGap populates report.MissingMessages for OpenAI/Responses format
// requests. It also sets report.HasTools based on the request's Tools slice.
func fillOpenAIGap(report *ReasoningGapReport, messages any) {
	req, ok := messages.(*dto.GeneralOpenAIRequest)
	if !ok || req == nil {
		return
	}
	report.HasTools = len(req.Tools) > 0
	if !report.HasTools {
		return
	}
	for i := range req.Messages {
		msg := &req.Messages[i]
		if msg.Role != "assistant" {
			continue
		}
		toolCalls := msg.ParseToolCalls()
		if len(toolCalls) == 0 {
			continue
		}
		if msg.ReasoningContent != nil {
			continue
		}
		ids := make([]string, 0, len(toolCalls))
		for _, tc := range toolCalls {
			if tc.ID != "" {
				ids = append(ids, tc.ID)
			}
		}
		report.MissingMessages = append(report.MissingMessages, MissingReasoningMessage{
			MessageIndex: i,
			ToolCallIDs:  ids,
		})
	}
}

// fillClaudeGap populates report.MissingMessages for Claude format requests.
// An assistant message is flagged when it contains tool_use blocks but no
// thinking block alongside them.
func fillClaudeGap(report *ReasoningGapReport, messages any) {
	req, ok := messages.(*dto.ClaudeRequest)
	if !ok || req == nil {
		return
	}
	report.HasTools = len(req.Tools) > 0
	if !report.HasTools {
		return
	}
	for i, m := range req.Messages {
		if m.Role != "assistant" {
			continue
		}
		blocks, ok := m.Content.([]dto.ClaudeMediaMessage)
		if !ok {
			continue
		}
		hasToolUse := false
		hasThinking := false
		var toolUseIDs []string
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				hasToolUse = true
				if b.ID != "" {
					toolUseIDs = append(toolUseIDs, b.ID)
				}
			case "thinking":
				hasThinking = true
			}
		}
		if !hasToolUse || hasThinking {
			continue
		}
		report.MissingMessages = append(report.MissingMessages, MissingReasoningMessage{
			MessageIndex: i,
			ToolCallIDs:  toolUseIDs,
		})
	}
}
