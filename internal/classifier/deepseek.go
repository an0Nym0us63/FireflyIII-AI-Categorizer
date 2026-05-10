package classifier

const deepseekBaseURL = "https://api.deepseek.com/v1"

// NewDeepseek returns an OpenAI-compatible classifier pointed at Deepseek's API.
func NewDeepseek(apiKey, model, customContext string) *OpenAIClassifier {
	return NewOpenAI(apiKey, model, deepseekBaseURL, customContext)
}
