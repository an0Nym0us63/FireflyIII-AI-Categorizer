package classifier

// BuildParams holds the fields needed to construct any Classifier implementation.
type BuildParams struct {
	Provider        string
	OpenAIKey       string
	OpenAIModel     string
	OpenAIBaseURL   string
	GeminiKey       string
	GeminiModel     string
	GeminiThinking  string
	GeminiGrounding bool
	DeepseekKey     string
	DeepseekModel   string
	CustomContext   string // optional free-text context appended to the system prompt
}

// Build constructs the appropriate Classifier for the given provider.
func Build(p BuildParams) (Classifier, error) {
	switch p.Provider {
	case "gemini":
		return NewGemini(p.GeminiKey, p.GeminiModel, p.CustomContext, p.GeminiThinking, p.GeminiGrounding)
	case "deepseek":
		return NewDeepseek(p.DeepseekKey, p.DeepseekModel, p.CustomContext), nil
	default:
		return NewOpenAI(p.OpenAIKey, p.OpenAIModel, p.OpenAIBaseURL, p.CustomContext), nil
	}
}
