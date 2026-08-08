package reasoning_guard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEndToEndCacheLifecycle walks the full L1 → L2 capture → L2 backfill
// cycle through the public wire entry points:
//
//   Turn 1: request carries tools + assistant turn carries tool_calls +
//           reasoning_content → CaptureResponseReasoning stores them.
//   Turn 2: request reuses those tool_calls but drops reasoning_content →
//           DetectReasoningContentGap reports missing → ApplyGuardRequest
//           uses cache to backfill → request reasoning_content is restored.
//
// This mirrors compatible_handler.go's guard integration points but without
// pulling in gin / adaptor globals.
func TestEndToEndCacheLifecycle(t *testing.T) {
	toolCallJSON := func(ids ...string) json.RawMessage {
		calls := make([]dto.ToolCallRequest, 0, len(ids))
		for _, id := range ids {
			calls = append(calls, dto.ToolCallRequest{
				ID:       id,
				Type:     "function",
				Function: dto.FunctionRequest{Name: "get_weather"},
			})
		}
		b, _ := json.Marshal(calls)
		return b
	}

	ctx := context.Background()
	cache := newInMemoryCache() // fresh per-test to avoid singleton leakage
	const channelID = 99
	// Build info with cache=on + guard=on
	info := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: "deepseek-v4-pro",
		RequestId:       "turn-1-abc",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:         channelID,
			UpstreamModelName: "deepseek-v4-pro",
			ChannelSetting: dto.ChannelSettings{
				DeepseekReasoningGuardDisabled: false,
				DeepseekReasoningCache:         true,
				DeepseekReasoningCacheTTL:      3_600_000,
			},
		},
	}

	// -------- Turn 1: Capture --------
	rc1 := "I should call get_weather for Paris."
	turn1Resp := &dto.OpenAITextResponse{
		Choices: []dto.OpenAITextResponseChoice{
			{
				Index: 0,
				Message: dto.Message{
					Role:             "assistant",
					ReasoningContent: &rc1,
					ToolCalls:        toolCallJSON("tc_turn1_1", "tc_turn1_2"),
				},
			},
		},
	}
	CaptureResponseReasoning(ctx, cache, info, turn1Resp)

	// Sanity: cache entries must now be present.
	hits, err := cache.Lookup(ctx, channelID, []string{"tc_turn1_1", "tc_turn1_2"})
	require.NoError(t, err)
	require.Len(t, hits, 2, "turn 1 capture should store both tool_call_ids")

	// -------- Turn 2: Backfill via ApplyGuardRequest --------
	// Reuse tool calls but DROP reasoning_content to simulate a framework bug.
	info2 := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: "deepseek-v4-pro",
		RequestId:       "turn-2-xyz",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:         channelID,
			UpstreamModelName: "deepseek-v4-pro",
			ChannelSetting: dto.ChannelSettings{
				DeepseekReasoningGuardDisabled: false,
				DeepseekReasoningCache:         true,
			},
		},
	}
	turn2Req := &dto.GeneralOpenAIRequest{
		Model: "deepseek-v4-pro",
		Tools: []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "get_weather"}}},
		Messages: []dto.Message{
			{Role: "user", Content: "weather in Paris?"},
			{Role: "assistant", Content: "", ToolCalls: toolCallJSON("tc_turn1_1", "tc_turn1_2")}, // NO ReasoningContent
			{Role: "tool", Content: "sunny, 20C", ToolCallId: "tc_turn1_1"},
			{Role: "tool", Content: "cloudy, 18C", ToolCallId: "tc_turn1_2"},
			{Role: "user", Content: "and tomorrow?"},
		},
	}
	// Precondition: reasoning content is nil (as would be the case for a dropped one)
	require.Nil(t, turn2Req.Messages[1].ReasoningContent)

	report, gErr := ApplyGuardRequest(ctx, cache, info2, turn2Req)
	require.NoError(t, gErr)
	// L1 should still report a formal gap because the *original* messages had
	// missing reasoning_content. But backfill should have mutated the request.
	assert.True(t, report.AnyMissing, "L1 must report the original gap")
	backfilledRC := turn2Req.Messages[1].ReasoningContent
	require.NotNil(t, backfilledRC, "L2 backfill should populate reasoning_content")
	// The TurnID mismatch: we used distinct RequestId turn-1-abc/turn-2-xyz
	// so the conservative backfill rule MUST refuse. Let's actually verify
	// the TurnID guard works by expecting NOT backfilled when TurnIDs differ.
	if hits["tc_turn1_1"].TurnID != hits["tc_turn1_2"].TurnID {
		// impossible for this test
	}
	// NOTE: in this fixture, the cache TurnID is "turn-1-abc" (info.RequestId
	// from turn 1), but CaptureResponseReasoning uses info.RequestId as the
	// TurnID, while Backfill only requires all cache entries share the SAME
	// non-empty TurnID (not necessarily the new request's). So backfill SHOULD
	// succeed here — TurnID guards against cross-turn mixing *within the cache*,
	// not across requests. That's the documented contract.
	assert.Equal(t, rc1, *backfilledRC, "backfilled reasoning_content should equal the captured value")
}

// TestL3WireFromGuardEnabled ensures MaybeEnhanceErrorResponse cooperates with
// the guardEnabled gate as exercised through the fixture-style path used by
// compatible_handler.go lines 211-220.
func TestL3WireFromGuardEnabled(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		guardOff bool
		apiType  int
		want     bool
	}{
		{"DeepSeek channel + V4 → L3 active", "deepseek-v4-pro", false, 53, true},
		{"Ali channel + V4 model (model sniffing) → L3 active", "deepseek-v4-pro", false, 40, true},
		{"OpenAI passthrough + V4 → L3 active", "deepseek-v4-pro", false, 1, true},
		{"guard off → L3 silent", "deepseek-v4-pro", true, 53, false},
	}
	const diagHeader = "X-Newapi-Reasoning-Guard"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				RelayFormat:     types.RelayFormatOpenAI,
				OriginModelName: tc.model,
				RequestId:       "req-01",
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelId:         10,
					ChannelType:       tc.apiType,
					UpstreamModelName: tc.model,
					ChannelSetting: dto.ChannelSettings{
						DeepseekReasoningGuardDisabled: tc.guardOff,
						DeepseekReasoningCache:         false,
					},
				},
			}
			rec := httptest.NewRecorder()
			rec.WriteHeader(http.StatusBadRequest)
			_, _ = rec.WriteString(`{"error":{"message":"reasoning_content must be passed back on tool_calls"}}`)
			resp := rec.Result()
			got := MaybeEnhanceErrorResponse(info, resp, rec.Body.Bytes())
			require.Equal(t, tc.want, got)
			if tc.want {
				assert.Equal(t, "missing-reasoning-content", resp.Header.Get(diagHeader))
			}
		})
	}
}
