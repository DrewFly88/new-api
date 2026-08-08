package reasoning_guard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildRelayInfo is a fixture helper for constructing RelayInfo + ChannelMeta
// with the model name and reasoning-guard channel settings.
func buildRelayInfo(t *testing.T, upstreamModel string, guardDisabled, cacheEnabled bool, cacheTTLMs int64) *relaycommon.RelayInfo {
	t.Helper()
	return &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: upstreamModel,
		RequestId:       "req-test-001",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:         42,
			UpstreamModelName: upstreamModel,
			ChannelSetting: dto.ChannelSettings{
				DeepseekReasoningGuardDisabled: guardDisabled,
				DeepseekReasoningCache:         cacheEnabled,
				DeepseekReasoningCacheTTL:      cacheTTLMs,
			},
		},
	}
}

// ---------------------------------------------------------------------------
// TestMaybeEnhanceErrorResponse (I5 L3 guidance)
// ---------------------------------------------------------------------------

func TestMaybeEnhanceErrorResponse(t *testing.T) {
	const diagHeader = "X-Newapi-Reasoning-Guard"
	const diagValue = "missing-reasoning-content"

	cases := []struct {
		name        string
		info        *relaycommon.RelayInfo
		statusCode  int
		body        string
		wantSet     bool
	}{
		{
			name:       "400 + reasoning_content mention + V4 → sets header",
			info:       buildRelayInfo(t, "deepseek-v4-pro", false, false, 0),
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"message":"reasoning_content must be passed back"}}`,
			wantSet:    true,
		},
		{
			name:       "400 + reasoning_content + non-V4 → noop",
			info:       buildRelayInfo(t, "deepseek-chat", false, false, 0),
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"message":"reasoning_content must be passed back"}}`,
			wantSet:    false,
		},
		{
			name:       "200 + reasoning_content mention → noop (non-400)",
			info:       buildRelayInfo(t, "deepseek-v4-pro", false, false, 0),
			statusCode: http.StatusOK,
			body:       `{"error":{"message":"reasoning_content diagnostic"}}`,
			wantSet:    false,
		},
		{
			name:       "400 + unrelated body → noop",
			info:       buildRelayInfo(t, "deepseek-v4-pro", false, false, 0),
			statusCode: http.StatusBadRequest,
			body:       `{"error":"invalid api key"}`,
			wantSet:    false,
		},
		{
			name:       "nil httpResp → noop",
			info:       buildRelayInfo(t, "deepseek-v4-pro", false, false, 0),
			statusCode: 0,
			body:       "",
			wantSet:    false,
		},
		{
			name:       "guard disabled → noop even when 400 + V4 + mention",
			info:       buildRelayInfo(t, "deepseek-v4-pro", true, false, 0),
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"message":"reasoning_content must be passed back"}}`,
			wantSet:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var httpResp *http.Response
			if tc.statusCode != 0 || tc.body != "" || tc.name == "nil httpResp" {
				if tc.statusCode == 0 && tc.body == "" && tc.name == "nil httpResp" {
					httpResp = nil
				} else {
					rec := httptest.NewRecorder()
					rec.WriteHeader(tc.statusCode)
					_, _ = rec.WriteString(tc.body)
					httpResp = rec.Result()
				}
			}
			bodyBytes := []byte(tc.body)
			got := MaybeEnhanceErrorResponse(tc.info, httpResp, bodyBytes)
			require.Equal(t, tc.wantSet, got, "MaybeEnhanceErrorResponse return mismatch")
			if tc.wantSet {
				require.NotNil(t, httpResp, "httpResp expected to be non-nil when header set")
				assert.Equal(t, diagValue, httpResp.Header.Get(diagHeader))
			} else if httpResp != nil {
				assert.Empty(t, httpResp.Header.Get(diagHeader), "diagnostic header should not be present")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestGuardEnabled / TestCacheEnabled / TestCacheTTL (I6 channel switch)
// ---------------------------------------------------------------------------

func TestGuardEnabled(t *testing.T) {
	cases := []struct {
		name         string
		info         *relaycommon.RelayInfo
		want         bool
	}{
		{"V4 + no opt-out → on", buildRelayInfo(t, "deepseek-v4-pro", false, false, 0), true},
		{"V4 + explicit opt-out → off", buildRelayInfo(t, "deepseek-v4-pro", true, false, 0), false},
		{"non-V4 model → off (ignores switch)", buildRelayInfo(t, "gpt-4o", false, false, 0), false},
		{"nil info → off", nil, false},
		{"nil ChannelMeta → off", func() *relaycommon.RelayInfo {
			info := buildRelayInfo(t, "deepseek-v4-pro", false, false, 0)
			info.ChannelMeta = nil
			return info
		}(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, guardEnabled(tc.info))
		})
	}
}

func TestCacheEnabled(t *testing.T) {
	cases := []struct {
		name string
		info *relaycommon.RelayInfo
		want bool
	}{
		{"cache=on + guard=on → true", buildRelayInfo(t, "deepseek-v4-pro", false, true, 0), true},
		{"cache=off → false", buildRelayInfo(t, "deepseek-v4-pro", false, false, 0), false},
		{"cache=on + guard=off → false (cache requires guard)", buildRelayInfo(t, "deepseek-v4-pro", true, true, 0), false},
		{"cache=on + non-V4 → false", buildRelayInfo(t, "gpt-4o", false, true, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cacheEnabled(tc.info))
		})
	}
}

func TestCacheTTL(t *testing.T) {
	cases := []struct {
		name    string
		ttlMs   int64
		want    time.Duration
	}{
		{"7,200,000 ms → 2h", 7_200_000, 2 * time.Hour},
		{"0 (unset) → 1h default", 0, time.Hour},
		{"-1 (invalid) → 1h default", -1, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := buildRelayInfo(t, "deepseek-v4-pro", false, true, tc.ttlMs)
			assert.Equal(t, tc.want, cacheTTL(info))
		})
	}
}

// ---------------------------------------------------------------------------
// TestDefaultCacheSingleton (I8 process-level singleton)
// ---------------------------------------------------------------------------

func TestDefaultCacheSingleton(t *testing.T) {
	// Multiple calls must return the same instance pointer.
	a := DefaultCache()
	b := DefaultCache()
	require.Same(t, a, b, "DefaultCache must return same instance across calls")

	// Accumulation: Store via first ref, Lookup via second ref.
	ctx := context.Background()
	err := a.Store(ctx, 7, "tc_singleton_1", "rc-singleton", "turnS", time.Hour)
	require.NoError(t, err)
	hits, err := b.Lookup(ctx, 7, []string{"tc_singleton_1"})
	require.NoError(t, err)
	require.Len(t, hits, 1, "cache must accumulate entries across callers")
	assert.Equal(t, "rc-singleton", hits["tc_singleton_1"].ReasoningContent)
}

// ---------------------------------------------------------------------------
// TestCaptureResponseReasoning (L2 capture path)
// ---------------------------------------------------------------------------

func TestCaptureResponseReasoning(t *testing.T) {
	toolCallJSON := func(ids ...string) json.RawMessage {
		calls := make([]dto.ToolCallRequest, 0, len(ids))
		for _, id := range ids {
			calls = append(calls, dto.ToolCallRequest{
				ID:   id,
				Type: "function",
				Function: dto.FunctionRequest{
					Name: "fn",
				},
			})
		}
		b, _ := json.Marshal(calls)
		return b
	}

	buildResp := func(rc string, toolCalls json.RawMessage) *dto.OpenAITextResponse {
		return &dto.OpenAITextResponse{
			Choices: []dto.OpenAITextResponseChoice{
				{
					Index: 0,
					Message: dto.Message{
						Role:             "assistant",
						ReasoningContent: &rc,
						ToolCalls:        toolCalls,
					},
				},
			},
		}
	}

	t.Run("cache enabled + rc + tool_calls → stores", func(t *testing.T) {
		info := buildRelayInfo(t, "deepseek-v4-pro", false, true, 0)
		cache := newInMemoryCache()
		ctx := context.Background()
		resp := buildResp("my-reasoning", toolCallJSON("call_cap_1", "call_cap_2"))
		CaptureResponseReasoning(ctx, cache, info, resp)
		hits, err := cache.Lookup(ctx, 42, []string{"call_cap_1", "call_cap_2"})
		require.NoError(t, err)
		require.Len(t, hits, 2, "both tool calls should be cached")
		for id, e := range hits {
			assert.Equal(t, "my-reasoning", e.ReasoningContent, "rc mismatch for %s", id)
			assert.Equal(t, "req-test-001", e.TurnID, "turn id must come from RequestId")
		}
	})

	t.Run("cache enabled + no rc → skip", func(t *testing.T) {
		info := buildRelayInfo(t, "deepseek-v4-pro", false, true, 0)
		cache := newInMemoryCache()
		ctx := context.Background()
		emptyRC := ""
		resp := &dto.OpenAITextResponse{
			Choices: []dto.OpenAITextResponseChoice{
				{
					Message: dto.Message{
						Role:             "assistant",
						ReasoningContent: &emptyRC, // empty string, GetReasoningContent returns ""
						ToolCalls:        toolCallJSON("call_cap_3"),
					},
				},
			},
		}
		CaptureResponseReasoning(ctx, cache, info, resp)
		hits, _ := cache.Lookup(ctx, 42, []string{"call_cap_3"})
		assert.Empty(t, hits, "empty reasoning_content must not be stored")
	})

	t.Run("cache enabled + no tool_calls → skip", func(t *testing.T) {
		info := buildRelayInfo(t, "deepseek-v4-pro", false, true, 0)
		cache := newInMemoryCache()
		ctx := context.Background()
		rc := "thinking without tool calls"
		resp := &dto.OpenAITextResponse{
			Choices: []dto.OpenAITextResponseChoice{
				{Message: dto.Message{Role: "assistant", ReasoningContent: &rc}},
			},
		}
		CaptureResponseReasoning(ctx, cache, info, resp)
		// nothing to assert via lookup — just no crash
	})

	t.Run("cache disabled → skip", func(t *testing.T) {
		info := buildRelayInfo(t, "deepseek-v4-pro", false, false, 0)
		cache := newInMemoryCache()
		ctx := context.Background()
		resp := buildResp("my-reasoning", toolCallJSON("call_cap_4"))
		CaptureResponseReasoning(ctx, cache, info, resp)
		hits, _ := cache.Lookup(ctx, 42, []string{"call_cap_4"})
		assert.Empty(t, hits, "when cache disabled nothing should be stored")
	})

	t.Run("nil resp → skip", func(t *testing.T) {
		info := buildRelayInfo(t, "deepseek-v4-pro", false, true, 0)
		cache := newInMemoryCache()
		// just check it doesn't panic
		CaptureResponseReasoning(context.Background(), cache, info, nil)
	})
}

// ---------------------------------------------------------------------------
// TestCaptureRace is a concurrency smoke test on in-memory cache store+lookup
// under CaptureResponseReasoning (reuses the race detector for thread safety).
// ---------------------------------------------------------------------------

func TestCaptureRace(t *testing.T) {
	info := buildRelayInfo(t, "deepseek-v4-pro", false, true, 0)
	cache := newInMemoryCache()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := "race_call_" + string(rune('a'+idx%26))
			rc := "race_rc_" + string(rune('a'+idx%26))
			tcJSON, _ := json.Marshal([]dto.ToolCallRequest{{
				ID: id, Type: "function", Function: dto.FunctionRequest{Name: "f"},
			}})
			resp := &dto.OpenAITextResponse{
				Choices: []dto.OpenAITextResponseChoice{
					{Message: dto.Message{
						Role:             "assistant",
						ReasoningContent: &rc,
						ToolCalls:        tcJSON,
					}},
				},
			}
			CaptureResponseReasoning(ctx, cache, info, resp)
			_, _ = cache.Lookup(ctx, 42, []string{id})
		}(i)
	}
	wg.Wait()
}
