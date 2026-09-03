package classifier

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

type GeminiClassifier struct {
	client        *genai.Client
	model         string
	customContext string
	thinking      string // "minimal" | "low" | "medium" | "high"
}

func NewGemini(apiKey, model, customContext, thinking string) (*GeminiClassifier, error) {
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini client: %w", err)
	}
	if thinking == "" {
		thinking = "low"
	}
	return &GeminiClassifier{
		client:        client,
		model:         model,
		customContext: customContext,
		thinking:      strings.ToLower(strings.TrimSpace(thinking)),
	}, nil
}

// thinkingConfig builds the right thinking setting for the target model.
// Gemini 3+ uses thinking_level; Gemini 2.5 uses thinking_budget (0 = off).
// The two are mutually exclusive per Google's API.
func (c *GeminiClassifier) thinkingConfig() *genai.ThinkingConfig {
	if strings.Contains(c.model, "gemini-2") {
		// Legacy 2.5 family: budget-based. Minimal/low → disable thinking.
		if c.thinking == "minimal" || c.thinking == "low" || c.thinking == "off" {
			return &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](0)}
		}
		return nil // leave dynamic thinking on for medium/high
	}
	// Gemini 3+ (and future): level-based.
	level := genai.ThinkingLevelLow
	switch c.thinking {
	case "medium":
		level = genai.ThinkingLevelMedium
	case "high":
		level = genai.ThinkingLevelHigh
	default: // minimal, low, off, unknown → lowest available level
		level = genai.ThinkingLevelLow
	}
	return &genai.ThinkingConfig{ThinkingLevel: level}
}

func (c *GeminiClassifier) Classify(ctx context.Context, req Request) (Result, error) {
	prompt := buildUserPrompt(req)

	sysPrompt := BuildSystemPrompt(c.customContext, req.DestinationMatching)
	if req.SystemPromptOverride != "" {
		sysPrompt = req.SystemPromptOverride
	}

	config := &genai.GenerateContentConfig{
		Temperature:      genai.Ptr[float32](0.1),
		ResponseMIMEType: "application/json",
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{{Text: sysPrompt}},
		},
		ThinkingConfig: c.thinkingConfig(),
	}

	resp, err := c.client.Models.GenerateContent(ctx, c.model, genai.Text(prompt), config)
	if err != nil {
		return Result{}, fmt.Errorf("gemini: %w", err)
	}

	text := resp.Text()
	if strings.TrimSpace(text) == "" {
		return Result{}, fmt.Errorf("gemini: empty response")
	}

	return parseResponse(text, prompt, req), nil
}
