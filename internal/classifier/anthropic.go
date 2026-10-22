package classifier

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type AnthropicClassifier struct {
	client        anthropic.Client
	model         string
	customContext string
	effort        string // "low" | "medium" | "high"
	webSearch     bool   // let Claude look up obscure merchants on the web
}

func NewAnthropic(apiKey, model, customContext, effort string, webSearch bool) *AnthropicClassifier {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		effort = "low"
	}
	return &AnthropicClassifier{
		client:        anthropic.NewClient(option.WithAPIKey(apiKey)),
		model:         model,
		customContext: customContext,
		effort:        effort,
		webSearch:     webSearch,
	}
}

// webSearchTool picks the web search variant the model supports: the dynamic
// filtering one (20260209) on Opus/Sonnet, the basic one elsewhere (Haiku).
func (c *AnthropicClassifier) webSearchTool() anthropic.ToolUnionParam {
	if strings.Contains(c.model, "opus") || strings.Contains(c.model, "sonnet") {
		return anthropic.ToolUnionParam{OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{
			MaxUses: anthropic.Int(3),
		}}
	}
	return anthropic.ToolUnionParam{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{
		MaxUses: anthropic.Int(3),
	}}
}

// maxPauseTurns bounds how many times a web-searching turn is resumed after the
// API pauses it (stop_reason "pause_turn").
const maxPauseTurns = 3

func (c *AnthropicClassifier) Classify(ctx context.Context, req Request) (Result, error) {
	prompt := buildUserPrompt(req)

	sysPrompt := BuildSystemPrompt(c.customContext, req.DestinationMatching)
	if req.SystemPromptOverride != "" {
		sysPrompt = req.SystemPromptOverride
	}
	if c.webSearch {
		// Same decision order as Gemini grounding: history first, then search.
		sysPrompt += "\n\n" + groundingInstruction
	}

	// No temperature: Claude 4.7+ models reject any value other than the default.
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 8000,
		System: []anthropic.TextBlockParam{{
			Text:         sysPrompt,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(c.effort),
		},
	}
	if c.webSearch {
		params.Tools = []anthropic.ToolUnionParam{c.webSearchTool()}
	}

	var resp *anthropic.Message
	for turn := 0; ; turn++ {
		var err error
		resp, err = c.client.Messages.New(ctx, params)
		if err != nil {
			return Result{}, fmt.Errorf("anthropic: %w", err)
		}
		if resp.StopReason != anthropic.StopReasonPauseTurn || turn >= maxPauseTurns {
			break
		}
		// A long web search was paused server-side: send the partial turn back
		// as-is so Claude resumes where it stopped.
		params.Messages = append(params.Messages, resp.ToParam())
	}

	if resp.StopReason == anthropic.StopReasonRefusal {
		return Result{}, fmt.Errorf("anthropic: request refused")
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if b, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	raw := strings.TrimSpace(text.String())
	if raw == "" {
		return Result{}, fmt.Errorf("anthropic: empty response (stop reason %q)", resp.StopReason)
	}

	return parseResponse(extractJSONObject(raw), prompt, req), nil
}

// extractJSONObject keeps the outermost {...} of the answer. With web search
// on, Claude may write a sentence about what it found before the JSON.
func extractJSONObject(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}
