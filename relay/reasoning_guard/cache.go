package reasoning_guard

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// cacheEntry is the stored value: reasoning_content plus a turn tag used by
// the conservative backfill to reject cross-turn or cross-session mixups.
type cacheEntry struct {
	ReasoningContent string    `json:"rc"`
	TurnID           string    `json:"turn"`
	CreatedAt        time.Time `json:"ts"`
}

// ReasoningContentCache caches DeepSeek V4 reasoning_content across requests
// so missing fields can be conservatively backfilled (L2).
//
// Conservative backfill policy (aligned with deepseek-compat-kit
// DSK_REASONING_003):
//  1. Every tool_call_id of the assistant message must have a cache hit.
//  2. All hits must share the same TurnID (same upstream turn).
//  3. The cache entries must not be expired.
//
// Two implementations are provided:
//   - inMemoryCache: sync.Map with a TTL; suitable for single-instance deploys.
//   - redisCache: backed by common.RDB; preferred when Redis is enabled and
//     the deployment has multiple instances.
//
// Default factory NewCache selects Redis when available, otherwise in-memory.
type ReasoningContentCache interface {
	// Store caches reasoning_content keyed by toolCallID for a given channel.
	// ttl is the per-entry time-to-live; TurnID tags the originating upstream
	// turn so Backfill can reject cross-turn mixups.
	Store(ctx context.Context, channelID int, toolCallID, reasoningContent, turnID string, ttl time.Duration) error

	// Lookup returns a map[toolCallID]cacheEntry for the IDs that are present
	// and unexpired. Missing IDs are omitted from the returned map.
	Lookup(ctx context.Context, channelID int, toolCallIDs []string) (map[string]cacheEntry, error)
}

// NewCache returns the default cache implementation: Redis when
// common.RedisEnabled && common.RDB != nil, otherwise in-memory.
func NewCache() ReasoningContentCache {
	if common.RedisEnabled && common.RDB != nil {
		return &redisCache{}
	}
	return newInMemoryCache()
}

// Backfill is the L2 conservative-backfill entry point. Given a gap report it
// attempts to backfill missing reasoning_content from cache. It mutates the
// request only when every tool_call_id of a missing message has a same-turn
// cache hit; otherwise it leaves the request untouched and returns nil.
//
// Currently supports OpenAI/Responses format (writes Message.ReasoningContent)
// and Claude format (injects a thinking content block ahead of tool_use blocks).
func Backfill(ctx context.Context, cache ReasoningContentCache, report ReasoningGapReport, channelID int, messages any) error {
	if cache == nil || !report.AnyMissing || len(report.MissingMessages) == 0 {
		return nil
	}
	switch report.RelayFormat {
	case types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses:
		return backfillOpenAI(ctx, cache, report, channelID, messages)
	case types.RelayFormatClaude:
		return backfillClaude(ctx, cache, report, channelID, messages)
	default:
		return nil
	}
}

func backfillOpenAI(ctx context.Context, cache ReasoningContentCache, report ReasoningGapReport, channelID int, messages any) error {
	req, ok := messages.(*dto.GeneralOpenAIRequest)
	if !ok || req == nil {
		return nil
	}
	for _, miss := range report.MissingMessages {
		if miss.MessageIndex < 0 || miss.MessageIndex >= len(req.Messages) {
			continue
		}
		// Guard against empty ToolCallIDs: a tool_use block without an Id
		// yields MissingReasoningMessage{ToolCallIDs: nil}. Without this
		// check the lookup-hits==ids gate below passes (0==0), the turn loop
		// iterates nothing, and we'd inject a thinking block with empty Text
		// — violating the conservative "leave untouched" contract.
		if len(miss.ToolCallIDs) == 0 {
			continue
		}
		hits, err := cache.Lookup(ctx, channelID, miss.ToolCallIDs)
		if err != nil {
			// Conservative: on lookup error, do not mutate; let upstream 400 flow.
			common.SysError(fmt.Sprintf("reasoning_guard cache lookup error: %v", err))
			continue
		}
		if len(hits) != len(miss.ToolCallIDs) {
			continue // partial or no hit → do not backfill
		}
		// All hits must share the same non-empty TurnID.
		turn := ""
		okTurn := true
		for _, e := range hits {
			if e.TurnID == "" {
				okTurn = false
				break
			}
			if turn == "" {
				turn = e.TurnID
			} else if e.TurnID != turn {
				okTurn = false
				break
			}
		}
		if !okTurn {
			continue
		}
		// Pick any hit's reasoning content (they share a turn).
		var rc string
		for _, e := range hits {
			rc = e.ReasoningContent
			break
		}
		msg := &req.Messages[miss.MessageIndex]
		msg.ReasoningContent = &rc
	}
	return nil
}

// backfillClaude is the Claude-format counterpart of backfillOpenAI.
//
// On a conservative hit (every tool_use id hit, same turn, unexpired) it
// injects a thinking content block at the HEAD of the assistant message's
// content array, ahead of any tool_use blocks. DeepSeek V4's Claude-format
// API expects thinking blocks to appear before tool_use in the same assistant
// turn, matching the order DeepSeek's own responses emit them.
//
// The injected block uses Type="thinking" with Text set to the cached
// reasoning_content. We do NOT set the Signature field — DeepSeek V4 does not
// require a thinking signature for tool-calling backfill, and fabricating one
// would risk upstream rejection.
func backfillClaude(ctx context.Context, cache ReasoningContentCache, report ReasoningGapReport, channelID int, messages any) error {
	req, ok := messages.(*dto.ClaudeRequest)
	if !ok || req == nil {
		return nil
	}
	for _, miss := range report.MissingMessages {
		if miss.MessageIndex < 0 || miss.MessageIndex >= len(req.Messages) {
			continue
		}
		// Guard against empty ToolCallIDs (see backfillOpenAI for rationale):
		// a tool_use block without an Id yields MissingReasoningMessage{ToolCallIDs: nil};
		// without this check we'd inject a thinking block with empty Text.
		if len(miss.ToolCallIDs) == 0 {
			continue
		}
		hits, err := cache.Lookup(ctx, channelID, miss.ToolCallIDs)
		if err != nil {
			common.SysError(fmt.Sprintf("reasoning_guard cache lookup error (claude): %v", err))
			continue
		}
		if len(hits) != len(miss.ToolCallIDs) {
			continue // partial or no hit → do not backfill
		}
		// All hits must share the same non-empty TurnID.
		turn := ""
		okTurn := true
		for _, e := range hits {
			if e.TurnID == "" {
				okTurn = false
				break
			}
			if turn == "" {
				turn = e.TurnID
			} else if e.TurnID != turn {
				okTurn = false
				break
			}
		}
		if !okTurn {
			continue
		}
		// Pick any hit's reasoning content (they share a turn).
		var rc string
		for _, e := range hits {
			rc = e.ReasoningContent
			break
		}
		msg := &req.Messages[miss.MessageIndex]
		blocks, ok := msg.Content.([]dto.ClaudeMediaMessage)
		if !ok {
			// Content is in an unexpected shape (e.g. plain string). Conservative:
			// do not mutate unknown content types — let upstream 400 flow and L3 guide.
			continue
		}
		// Inject thinking block at the head, ahead of tool_use blocks.
		thinkingBlock := dto.ClaudeMediaMessage{
			Type: "thinking",
			Text: &rc,
		}
		msg.Content = append([]dto.ClaudeMediaMessage{thinkingBlock}, blocks...)
	}
	return nil
}

// --- in-memory implementation ---

type inMemoryCache struct {
	mu    sync.Mutex
	store map[string]cacheEntry // key: channelID:toolCallID
}

func newInMemoryCache() *inMemoryCache {
	return &inMemoryCache{store: make(map[string]cacheEntry)}
}

func imKey(channelID int, toolCallID string) string {
	return fmt.Sprintf("%d:%s", channelID, toolCallID)
}

func (c *inMemoryCache) Store(_ context.Context, channelID int, toolCallID, reasoningContent, turnID string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[imKey(channelID, toolCallID)] = cacheEntry{
		ReasoningContent: reasoningContent,
		TurnID:           turnID,
		CreatedAt:        time.Now(),
	}
	// TTL is enforced lazily in Lookup via a deadline field; to keep the
	// struct small we spawn a goroutine to evict after ttl.
	if ttl > 0 {
		key := imKey(channelID, toolCallID)
		time.AfterFunc(ttl, func() {
			c.mu.Lock()
			delete(c.store, key)
			c.mu.Unlock()
		})
	}
	return nil
}

func (c *inMemoryCache) Lookup(_ context.Context, channelID int, toolCallIDs []string) (map[string]cacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]cacheEntry, len(toolCallIDs))
	for _, id := range toolCallIDs {
		e, ok := c.store[imKey(channelID, id)]
		if !ok {
			continue
		}
		out[id] = e
	}
	return out, nil
}

// --- Redis implementation ---

type redisCache struct{}

func rcKey(channelID int, toolCallID string) string {
	return fmt.Sprintf("newapi:dsreasoning:%d:%s", channelID, toolCallID)
}

func (c *redisCache) Store(ctx context.Context, channelID int, toolCallID, reasoningContent, turnID string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = time.Hour
	}
	e := cacheEntry{
		ReasoningContent: reasoningContent,
		TurnID:           turnID,
		CreatedAt:        time.Now(),
	}
	b, err := common.Marshal(e)
	if err != nil {
		return err
	}
	return common.RedisSet(rcKey(channelID, toolCallID), string(b), ttl)
}

func (c *redisCache) Lookup(ctx context.Context, channelID int, toolCallIDs []string) (map[string]cacheEntry, error) {
	out := make(map[string]cacheEntry, len(toolCallIDs))
	for _, id := range toolCallIDs {
		raw, err := common.RedisGet(rcKey(channelID, id))
		if err != nil || raw == "" {
			continue
		}
		var e cacheEntry
		if err := common.Unmarshal([]byte(raw), &e); err != nil {
			continue
		}
		out[id] = e
	}
	return out, nil
}
