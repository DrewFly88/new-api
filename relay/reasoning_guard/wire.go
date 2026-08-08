// Package reasoning_guard (wire) — relay-entry integration hooks.
//
// This file wires the reasoning_guard primitives into the relay entry layer:
//
//   - ApplyGuardRequest runs L1 diagnosis + L2 conservative backfill on an
//     inbound OpenAI/Claude/Responses request. It must be called AFTER the
//     upstream model name is resolved (so model-name sniffing is accurate)
//     and BEFORE the channel adaptor converts/forwards the request.
//   - MaybeEnhanceErrorResponse runs L3 client guidance: when upstream returns
//     HTTP 400 with the "reasoning_content must be passed back" message, it
//     injects a diagnostic header so clients can detect the condition.
//   - CaptureResponseReasoning runs L2 capture: stores the upstream response's
//     reasoning_content keyed by tool_call_id, so a later turn can backfill.
//
// The guard is gated by model-name prefix "deepseek-v4-" (DetectIfDeepSeekV4)
// and by the channel-level settings DeepseekReasoningGuard /
// DeepseekReasoningCache. It is intentionally channel-type agnostic.
package reasoning_guard

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	newapicommon "github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

// defaultCache is the process-wide L2 cache singleton.
//
// It MUST be a singleton (not constructed per-request) so the in-memory
// backend accumulates reasoning_content across requests — otherwise each
// request's cache would be empty and discarded, making L2 backfill a no-op.
// The Redis backend is stateless so sharing one instance is safe too.
//
// Use DefaultCache() at the call sites instead of NewCache().
var defaultCacheOnce sync.Once
var defaultCache ReasoningContentCache

// DefaultCache returns the process-wide cache instance, initializing it on
// first use (Redis when available, otherwise in-memory).
func DefaultCache() ReasoningContentCache {
	defaultCacheOnce.Do(func() {
		defaultCache = NewCache()
	})
	return defaultCache
}

// guardEnabled reports whether the guard is on for this channel+model.
//
// The guard defaults to ON for any channel carrying a deepseek-v4-* model.
// A channel that explicitly sets DeepseekReasoningGuardDisabled=true opts out
// (the separate opt-out flag keeps the default-on semantics legible: the
// zero-value bool means "not set", so we cannot reuse DeepseekReasoningGuard
// itself as an opt-out signal).
func guardEnabled(info *relaycommon.RelayInfo) bool {
	if info == nil || info.ChannelMeta == nil {
		return false
	}
	if !DetectIfDeepSeekV4(currentModelName(info)) {
		return false
	}
	return !info.ChannelSetting.DeepseekReasoningGuardDisabled
}

// cacheEnabled reports whether L2 server-side cache/backfill is on.
func cacheEnabled(info *relaycommon.RelayInfo) bool {
	if info == nil || info.ChannelMeta == nil {
		return false
	}
	return info.ChannelSetting.DeepseekReasoningCache && guardEnabled(info)
}

// cacheTTL returns the configured cache TTL, defaulting to 1 hour
// (aligned with deepseek-compat-kit) when unset.
func cacheTTL(info *relaycommon.RelayInfo) time.Duration {
	if info == nil || info.ChannelMeta == nil {
		return time.Hour
	}
	ms := info.ChannelSetting.DeepseekReasoningCacheTTL
	if ms <= 0 {
		return time.Hour
	}
	return time.Duration(ms) * time.Millisecond
}

// ApplyGuardRequest runs L1 diagnosis and L2 conservative backfill on an
// inbound request. It is safe to call with any request type; non-DeepSeek-V4
// models or channels with the guard disabled are no-ops.
//
// Returns the gap report so the caller can attach it to RelayInfo for the
// response phase (L3). The report is zero-valued when the guard is disabled.
func ApplyGuardRequest(ctx context.Context, cache ReasoningContentCache, info *relaycommon.RelayInfo, messages any) (ReasoningGapReport, error) {
	if !guardEnabled(info) {
		return ReasoningGapReport{}, nil
	}
	report := DetectReasoningContentGap(info.RelayFormat, currentModelName(info), messages)
	if !report.AnyMissing || !cacheEnabled(info) || cache == nil {
		return report, nil
	}
	if err := Backfill(ctx, cache, report, info.GetChannelID(), messages); err != nil {
		return report, err
	}
	return report, nil
}

func currentModelName(info *relaycommon.RelayInfo) string {
	if info == nil || info.ChannelMeta == nil {
		return ""
	}
	if info.UpstreamModelName != "" {
		return info.UpstreamModelName
	}
	return info.OriginModelName
}

// CaptureResponseReasoning runs L2 capture: it stores reasoning_content from
// an upstream non-streaming OpenAI/Responses response keyed by tool_call_id.
// It is a no-op when the channel cache is disabled or the model is not
// DeepSeek-V4.
//
// TurnID is an opaque tag distinguishing one upstream turn from another; the
// conservative backfill rejects cross-turn mixups using it. When the caller
// has no meaningful turn id, pass "" — backfill then requires all hits to
// share an empty TurnID, which still prevents cross-channel mixups via the
// channelID key namespace.
func CaptureResponseReasoning(ctx context.Context, cache ReasoningContentCache, info *relaycommon.RelayInfo, resp *dto.OpenAITextResponse) {
	if !cacheEnabled(info) || cache == nil || resp == nil {
		return
	}
	channelID := info.GetChannelID()
	ttl := cacheTTL(info)
	turnID := info.RequestId
	for _, choice := range resp.Choices {
		rc := choice.Message.GetReasoningContent()
		if rc == "" {
			continue
		}
		toolCalls := choice.Message.ParseToolCalls()
		if len(toolCalls) == 0 {
			continue
		}
		for _, tc := range toolCalls {
			if tc.ID == "" {
				continue
			}
			if err := cache.Store(ctx, channelID, tc.ID, rc, turnID, ttl); err != nil {
				newapicommon.SysError(fmt.Sprintf("reasoning_guard cache store error: %v", err))
			}
		}
	}
}

// MaybeEnhanceErrorResponse runs L3 client guidance. When the upstream
// response is HTTP 400 with a body mentioning "reasoning_content", it sets
// the response header X-Newapi-Reasoning-Guard: missing-reasoning-content so
// clients can detect the condition programmatically.
//
// It is safe to call with nil resp. Returns true when the response header was
// set.
func MaybeEnhanceErrorResponse(info *relaycommon.RelayInfo, httpResp *http.Response, body []byte) bool {
	if !guardEnabled(info) || httpResp == nil {
		return false
	}
	if httpResp.StatusCode != http.StatusBadRequest {
		return false
	}
	if !strings.Contains(strings.ToLower(string(body)), "reasoning_content") {
		return false
	}
	httpResp.Header.Set("X-Newapi-Reasoning-Guard", "missing-reasoning-content")
	return true
}

// CaptureStreamResponseReasoning is the streaming-response counterpart of
// CaptureResponseReasoning. It runs L2 capture for SSE-delivered responses
// where reasoning_content arrives in fragments across multiple chunks and
// tool_call ids may be split across chunks too.
//
// The caller (stream handler) is responsible for accumulating two slices
// during the stream scan and passing them here once the stream completes:
//   - reasoningContent: the concatenated reasoning_content fragments (already
//     merged by the handler in arrival order).
//   - toolCallIDs: the deduplicated, non-empty tool_call ids seen across all
//     chunks of this assistant turn.
//
// This split design keeps the guard out of the hot streaming loop — the
// handler accumulates with its existing scanner, then calls us once at the
// end, avoiding per-chunk cache writes and locking contention.
//
// No-op when channel cache is disabled, model is not DeepSeek-V4, or both
// inputs are empty. Store errors are logged via common.SysError and do not
// abort the caller's flow.
func CaptureStreamResponseReasoning(ctx context.Context, cache ReasoningContentCache, info *relaycommon.RelayInfo, reasoningContent string, toolCallIDs []string) {
	if !cacheEnabled(info) || cache == nil {
		return
	}
	if reasoningContent == "" || len(toolCallIDs) == 0 {
		return
	}
	channelID := info.GetChannelID()
	ttl := cacheTTL(info)
	turnID := info.RequestId
	for _, id := range toolCallIDs {
		if id == "" {
			continue
		}
		if err := cache.Store(ctx, channelID, id, reasoningContent, turnID, ttl); err != nil {
			newapicommon.SysError(fmt.Sprintf("reasoning_guard stream cache store error: %v", err))
		}
	}
}

// CollectStreamReasoning is a per-chunk helper for stream handlers. It parses
// one SSE chunk's reasoning_content fragment and tool_call ids, returning them
// so the handler can accumulate across chunks. Empty results are returned as
// zero values so the caller can skip no-op chunks.
//
// This is intentionally a pure parse helper — no cache I/O — so it can be
// called inside the hot streaming loop without lock contention. The handler
// merges the returned fragments in arrival order and feeds the final
// concatenation to CaptureStreamResponseReasoning once the stream ends.
//
// Callers SHOULD gate this on StreamGuardActive(info) computed once before the
// stream loop, so non-DeepSeek streaming traffic does not incur an extra
// per-chunk JSON parse.
func CollectStreamReasoning(chunk []byte) (reasoningFragment string, toolCallIDs []string) {
	var streamResp dto.ChatCompletionsStreamResponse
	if err := common.UnmarshalJsonStr(string(chunk), &streamResp); err != nil {
		return "", nil
	}
	for _, choice := range streamResp.Choices {
		if frag := choice.Delta.GetReasoningContent(); frag != "" {
			reasoningFragment += frag
		}
		for _, tc := range choice.Delta.ToolCalls {
			if tc.ID != "" {
				toolCallIDs = append(toolCallIDs, tc.ID)
			}
		}
	}
	return reasoningFragment, toolCallIDs
}

// StreamGuardActive reports whether the L2 streaming guard should accumulate
// per-chunk reasoning fragments for this request. Stream handlers SHOULD call
// this ONCE before the stream loop and skip CollectStreamReasoning entirely
// when it returns false, so non-DeepSeek streaming traffic does not pay an
// extra per-chunk JSON parse on the hot path.
//
// This is the streaming counterpart of the cacheEnabled predicate: it also
// requires the guard itself to be on (guardEnabled), since accumulating
// fragments when the guard is off would be wasted work feeding a no-op
// CaptureStreamResponseReasoning.
func StreamGuardActive(info *relaycommon.RelayInfo) bool {
	return cacheEnabled(info)
}
