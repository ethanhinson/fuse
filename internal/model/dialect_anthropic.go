package model

// anthropicDialect is the Claude family. Anthropic caches a prefix only up to a
// block marked cache_control, so without marks every call pays full input price
// for a history it has already seen (see markPromptCache). Every Claude model
// since 3 accepts images, so attachments default on.
var anthropicDialect = dialect{
	name:    DialectAnthropic,
	markers: []string{"anthropic", "claude", "opus", "sonnet", "haiku"},
	defaults: capabilities{
		cacheMarkers: true, recoverToolCalls: true, retryEmptyUnstreamed: true, attachments: true,
	},
}
