package model

const anthropicHistoryMarks = 2

// markPromptCache marks prompt-cache breakpoints in the Anthropic cache_control
// convention, used by any dialect whose cacheMarkers is on. LiteLLM and
// OpenRouter pass the marks through in the OpenAI content-part form used here
// (verified live: the second call reads the whole marked prefix from cache).
//
// It marks the system prompt, which also caches the tool definitions ahead of
// it, and the last two non-empty user or tool messages. The latest mark extends
// the cache to the whole history; the one before it gives the next call a cache
// read even when a turn adds more blocks than Anthropic's lookback reaches.
// That is three of the four marks Anthropic allows. Assistant messages are never
// marked, and neither is empty text, which Anthropic rejects.
func markPromptCache(req *wireReq) {
	for i := range req.Messages {
		if req.Messages[i].Role == "system" && req.Messages[i].Content != "" {
			req.Messages[i].markCacheBreakpoint()
			break
		}
	}
	marked := 0
	for i := len(req.Messages) - 1; i >= 0 && marked < anthropicHistoryMarks; i-- {
		m := &req.Messages[i]
		if (m.Role == "user" || m.Role == "tool") && (m.Content != "" || len(m.images) > 0) {
			m.markCacheBreakpoint()
			marked++
		}
	}
}
