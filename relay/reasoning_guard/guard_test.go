package reasoning_guard

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDetectIfDeepSeekV4 covers the model-name sniffing gate.
func TestDetectIfDeepSeekV4(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  bool
	}{
		{"v4-pro", "deepseek-v4-pro", true},
		{"v4-flash", "deepseek-v4-flash", true},
		{"v4-suffix-max", "deepseek-v4-pro-max", true},
		{"uppercase", "DeepSeek-V4-Pro", true},
		{"all-uppercase", "DEEPSEEK-V4-PRO", true},
		{"mixed-case", "deepSeek-v4-pro", true},
		{"infix-wrapped", "v4-deepseek-pro", true},
		{"no-dash", "deepseekv4-pro", true},
		{"marker-only", "deepseek-v4", true},
		{"legacy-chat", "deepseek-chat", false},
		{"legacy-reasoner", "deepseek-reasoner", false},
		{"unrelated", "gpt-4o", false},
		{"v3", "deepseek-v3-pro", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DetectIfDeepSeekV4(tc.model))
		})
	}
}

// TestDetectReasoningContentGapOpenAI is a table-driven test covering L1
// diagnosis for OpenAI-format requests across the documented invariants.
func TestDetectReasoningContentGapOpenAI(t *testing.T) {
	type tc struct {
		name        string
		model       string
		tools       []dto.ToolCallRequest
		messages    []dto.Message
		wantMissing int
		wantHasTools bool
		wantIsV4     bool
	}

	toolCallJSON := func(id string) json.RawMessage {
		b, _ := json.Marshal([]dto.ToolCallRequest{{ID: id, Type: "function", Function: dto.FunctionRequest{Name: "get_weather"}}})
		return b
	}

	rc := "I should inspect the weather."
	cases := []tc{
		{
			name:   "complete fields → no gap",
			model:  "deepseek-v4-pro",
			tools:  []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "get_weather"}}},
			messages: []dto.Message{
				{Role: "user", Content: "weather in Paris?"},
				{Role: "assistant", Content: "", ToolCalls: toolCallJSON("call_1"), ReasoningContent: &rc},
				{Role: "tool", Content: "sunny", ToolCallId: "call_1"},
				{Role: "user", Content: "and tomorrow?"},
			},
			wantMissing:  0,
			wantHasTools: true,
			wantIsV4:     true,
		},
		{
			name:   "missing reasoning_content on tool-call assistant → gap",
			model:  "deepseek-v4-pro",
			tools:  []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "get_weather"}}},
			messages: []dto.Message{
				{Role: "user", Content: "weather in Paris?"},
				{Role: "assistant", Content: "", ToolCalls: toolCallJSON("call_1")}, // no ReasoningContent
				{Role: "user", Content: "and tomorrow?"},
			},
			wantMissing:  1,
			wantHasTools: true,
			wantIsV4:     true,
		},
		{
			name:   "non-v4 model → skip detection",
			model:  "deepseek-chat",
			tools:  []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "get_weather"}}},
			messages: []dto.Message{
				{Role: "assistant", Content: "", ToolCalls: toolCallJSON("call_1")}, // would be a gap, but model gate skips
			},
			wantMissing:  0,
			wantHasTools: false,
			wantIsV4:     false,
		},
		{
			name:   "no tools → skip detection",
			model:  "deepseek-v4-pro",
			tools:  nil,
			messages: []dto.Message{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "hi"},
			},
			wantMissing:  0,
			wantHasTools: false,
			wantIsV4:     true,
		},
		{
			name:   "non-tool-call turn missing reasoning → not a gap (DeepSeek allows omitting reasoning_content on non-tool turns)",
			model:  "deepseek-v4-pro",
			tools:  []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "get_weather"}}},
			messages: []dto.Message{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "hi"}, // no tool_calls → not scanned
				{Role: "user", Content: "again"},
			},
			wantMissing:  0,
			wantHasTools: true,
			wantIsV4:     true,
		},
		{
			name:   "multiple tool-call turns, one missing → only the missing one is reported",
			model:  "deepseek-v4-pro",
			tools:  []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "get_weather"}}},
			messages: []dto.Message{
				{Role: "user", Content: "weather in Paris?"},
				{Role: "assistant", Content: "", ToolCalls: toolCallJSON("call_1"), ReasoningContent: &rc},
				{Role: "tool", Content: "sunny", ToolCallId: "call_1"},
				{Role: "user", Content: "and tomorrow?"},
				{Role: "assistant", Content: "", ToolCalls: toolCallJSON("call_2")}, // missing reasoning_content
			},
			wantMissing:  1,
			wantHasTools: true,
			wantIsV4:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &dto.GeneralOpenAIRequest{
				Model:    tc.model,
				Tools:    tc.tools,
				Messages: tc.messages,
			}
			report := DetectReasoningContentGap(types.RelayFormatOpenAI, tc.model, req)
			require.Equal(t, tc.wantIsV4, report.IsDeepSeekV4)
			require.Equal(t, tc.wantHasTools, report.HasTools)
			require.Len(t, report.MissingMessages, tc.wantMissing)
			assert.Equal(t, tc.wantMissing > 0, report.AnyMissing)
			// Verify ToolCallIDs are captured for missing messages.
			for _, m := range report.MissingMessages {
				assert.NotEmpty(t, m.ToolCallIDs, "missing message should record tool_call_ids")
			}
		})
	}
}

// TestDetectReasoningContentGapClaude covers L1 diagnosis for Claude-format
// requests (thinking content block presence on tool_use turns).
func TestDetectReasoningContentGapClaude(t *testing.T) {
	thinkingText := "let me think"
	toolUseID := "toolu_1"

	cases := []struct {
		name        string
		model       string
		hasThinking bool
		wantMissing int
	}{
		{"with thinking block → no gap", "deepseek-v4-pro", true, 0},
		{"without thinking block → gap", "deepseek-v4-pro", false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := []dto.ClaudeMediaMessage{
				{Type: "tool_use", ID: toolUseID, Name: "get_weather", Input: map[string]any{"city": "Paris"}},
			}
			if tc.hasThinking {
				blocks = append([]dto.ClaudeMediaMessage{{Type: "thinking", Text: &thinkingText}}, blocks...)
			}
			req := &dto.ClaudeRequest{
				Model:    tc.model,
				Messages: []dto.ClaudeMessage{{Role: "user", Content: "weather?"}, {Role: "assistant", Content: blocks}},
				Tools:    []dto.ClaudeTool{{Type: "custom", Name: "get_weather"}},
			}
			report := DetectReasoningContentGap(types.RelayFormatClaude, tc.model, req)
			require.True(t, report.IsDeepSeekV4)
			require.True(t, report.HasTools)
			require.Len(t, report.MissingMessages, tc.wantMissing)
		})
	}
}

// TestBackfillOpenAIConservative covers the L2 conservative-backfill policy:
// every tool_call_id must hit, all hits must share a TurnID, and the cache
// entry must be present. Partial hit → no mutation.
func TestBackfillOpenAIConservative(t *testing.T) {
	toolCallJSON := func(ids ...string) json.RawMessage {
		calls := make([]dto.ToolCallRequest, 0, len(ids))
		for _, id := range ids {
			calls = append(calls, dto.ToolCallRequest{ID: id, Type: "function", Function: dto.FunctionRequest{Name: "f"}})
		}
		b, _ := json.Marshal(calls)
		return b
	}

	t.Run("full hit same turn → backfill", func(t *testing.T) {
		cache := newInMemoryCache()
		ctx := context.Background()
		require.NoError(t, cache.Store(ctx, 1, "call_1", "rc1", "turnA", time.Hour))
		require.NoError(t, cache.Store(ctx, 1, "call_2", "rc2", "turnA", time.Hour))

		req := &dto.GeneralOpenAIRequest{
			Model: "deepseek-v4-pro",
			Tools: []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "f"}}},
			Messages: []dto.Message{
				{Role: "user", Content: "q"},
				{Role: "assistant", Content: "", ToolCalls: toolCallJSON("call_1", "call_2")}, // missing reasoning_content
			},
		}
		report := DetectReasoningContentGap(types.RelayFormatOpenAI, req.Model, req)
		require.True(t, report.AnyMissing)
		require.NoError(t, Backfill(ctx, cache, report, 1, req))
		require.NotNil(t, req.Messages[1].ReasoningContent, "reasoning_content should be backfilled")
	})

	t.Run("partial hit → no mutation", func(t *testing.T) {
		cache := newInMemoryCache()
		ctx := context.Background()
		require.NoError(t, cache.Store(ctx, 1, "call_1", "rc1", "turnA", time.Hour))
		// call_2 is NOT in cache.

		req := &dto.GeneralOpenAIRequest{
			Model: "deepseek-v4-pro",
			Tools: []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "f"}}},
			Messages: []dto.Message{
				{Role: "user", Content: "q"},
				{Role: "assistant", Content: "", ToolCalls: toolCallJSON("call_1", "call_2")},
			},
		}
		report := DetectReasoningContentGap(types.RelayFormatOpenAI, req.Model, req)
		require.NoError(t, Backfill(ctx, cache, report, 1, req))
		require.Nil(t, req.Messages[1].ReasoningContent, "partial hit must not backfill")
	})

	t.Run("cross-turn hit → no mutation", func(t *testing.T) {
		cache := newInMemoryCache()
		ctx := context.Background()
		require.NoError(t, cache.Store(ctx, 1, "call_1", "rc1", "turnA", time.Hour))
		require.NoError(t, cache.Store(ctx, 1, "call_2", "rc2", "turnB", time.Hour))

		req := &dto.GeneralOpenAIRequest{
			Model: "deepseek-v4-pro",
			Tools: []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "f"}}},
			Messages: []dto.Message{
				{Role: "user", Content: "q"},
				{Role: "assistant", Content: "", ToolCalls: toolCallJSON("call_1", "call_2")},
			},
		}
		report := DetectReasoningContentGap(types.RelayFormatOpenAI, req.Model, req)
		require.NoError(t, Backfill(ctx, cache, report, 1, req))
		require.Nil(t, req.Messages[1].ReasoningContent, "cross-turn hit must not backfill")
	})

	t.Run("no cache → no mutation", func(t *testing.T) {
		cache := newInMemoryCache()
		ctx := context.Background()
		req := &dto.GeneralOpenAIRequest{
			Model: "deepseek-v4-pro",
			Tools: []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "f"}}},
			Messages: []dto.Message{
				{Role: "user", Content: "q"},
				{Role: "assistant", Content: "", ToolCalls: toolCallJSON("call_1")},
			},
		}
		report := DetectReasoningContentGap(types.RelayFormatOpenAI, req.Model, req)
		require.NoError(t, Backfill(ctx, cache, report, 1, req))
		require.Nil(t, req.Messages[1].ReasoningContent, "empty cache must not backfill")
	})
}

// TestInMemoryCacheTTL verifies that the in-memory cache honors TTL eviction.
func TestInMemoryCacheTTL(t *testing.T) {
	cache := newInMemoryCache()
	ctx := context.Background()
	require.NoError(t, cache.Store(ctx, 1, "call_1", "rc1", "turnA", 50*time.Millisecond))

	hits, err := cache.Lookup(ctx, 1, []string{"call_1"})
	require.NoError(t, err)
	require.Len(t, hits, 1, "entry should be present before TTL expires")

	time.Sleep(100 * time.Millisecond)
	hits, err = cache.Lookup(ctx, 1, []string{"call_1"})
	require.NoError(t, err)
	require.Empty(t, hits, "entry should be evicted after TTL expires")
}
