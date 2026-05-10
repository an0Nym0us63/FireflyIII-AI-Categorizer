package classifier

import (
	"context"
	"fmt"

	openai "github.com/sashabaranov/go-openai"
)

type OpenAIClassifier struct {
	client        *openai.Client
	model         string
	customContext string
}

func NewOpenAI(apiKey, model, baseURL, customContext string) *OpenAIClassifier {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	return &OpenAIClassifier{
		client:        openai.NewClientWithConfig(cfg),
		model:         model,
		customContext: customContext,
	}
}

func (c *OpenAIClassifier) Classify(ctx context.Context, req Request) (Result, error) {
	prompt := buildUserPrompt(req)

	resp, err := c.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: c.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: BuildSystemPrompt(c.customContext)},
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
		Temperature: 0.1,
	})
	if err != nil {
		return Result{}, fmt.Errorf("openai: %w", err)
	}

	raw := resp.Choices[0].Message.Content
	return parseResponse(raw, prompt, req.Categories), nil
}
