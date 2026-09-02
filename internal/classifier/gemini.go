package classifier

import (
	"context"
	"fmt"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

type GeminiClassifier struct {
	client        *genai.Client
	model         string
	customContext string
}

func NewGemini(apiKey, model, customContext string) (*GeminiClassifier, error) {
	client, err := genai.NewClient(context.Background(), option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("gemini client: %w", err)
	}
	return &GeminiClassifier{client: client, model: model, customContext: customContext}, nil
}

func (c *GeminiClassifier) Classify(ctx context.Context, req Request) (Result, error) {
	prompt := buildUserPrompt(req)

	sysPrompt := BuildSystemPrompt(c.customContext, req.DestinationMatching)
	if req.SystemPromptOverride != "" {
		sysPrompt = req.SystemPromptOverride
	}

	m := c.client.GenerativeModel(c.model)
	temp := float32(0.1)
	m.Temperature = &temp
	m.ResponseMIMEType = "application/json"
	m.SystemInstruction = &genai.Content{
		Parts: []genai.Part{genai.Text(sysPrompt)},
	}

	resp, err := m.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		return Result{}, fmt.Errorf("gemini: %w", err)
	}

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return Result{}, fmt.Errorf("gemini: empty response")
	}

	text, ok := resp.Candidates[0].Content.Parts[0].(genai.Text)
	if !ok {
		return Result{}, fmt.Errorf("gemini: unexpected response part type")
	}

	return parseResponse(string(text), prompt, req), nil
}
