package model

// openAIDialect is the OpenAI-compatible family: providers cache automatically,
// so the request is sent as is.
var openAIDialect = dialect{
	name:     DialectOpenAI,
	defaults: capabilities{recoverToolCalls: true, retryEmptyUnstreamed: true},
}
