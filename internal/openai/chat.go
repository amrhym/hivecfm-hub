package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
)

var (
	// ErrEmptySentimentInput is returned when ClassifySentiment is called with empty input.
	ErrEmptySentimentInput = errors.New("openai: sentiment input text is empty")
	// ErrInvalidSentimentResponse is returned when the model response cannot be parsed.
	ErrInvalidSentimentResponse = errors.New("openai: invalid sentiment response")
)

const sentimentSystemPrompt = `You are a sentiment classifier. Given user feedback text, classify its sentiment.
Respond ONLY with a JSON object in this exact format, no other text:
{"sentiment": "positive", "score": 0.95}

Rules:
- "sentiment" must be one of: "positive", "negative", "neutral"
- "score" must be a float between 0.0 and 1.0 representing confidence
- Do not include any explanation or text outside the JSON object`

// sentimentResponse is the expected JSON structure from the model.
type sentimentResponse struct {
	Sentiment string  `json:"sentiment"`
	Score     float64 `json:"score"`
}

// ChatClient calls the OpenAI chat completions API for sentiment classification.
type ChatClient struct {
	sdk   openaisdk.Client
	model string
}

// NewChatClient creates a ChatClient for sentiment analysis.
func NewChatClient(apiKey string, model string) *ChatClient {
	return &ChatClient{
		sdk:   openaisdk.NewClient(option.WithAPIKey(apiKey)),
		model: model,
	}
}

// ClassifySentiment classifies the sentiment of the given text.
// Returns the sentiment label (positive/negative/neutral) and a confidence score (0-1).
func (c *ChatClient) ClassifySentiment(ctx context.Context, text string) (string, float64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", 0, ErrEmptySentimentInput
	}

	resp, err := c.sdk.Chat.Completions.New(ctx, openaisdk.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openaisdk.ChatCompletionMessageParamUnion{
			openaisdk.SystemMessage(sentimentSystemPrompt),
			openaisdk.UserMessage(text),
		},
		Temperature: param.NewOpt(0.0),
	})
	if err != nil {
		return "", 0, fmt.Errorf("openai chat: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", 0, ErrInvalidSentimentResponse
	}

	content := strings.TrimSpace(resp.Choices[0].Message.Content)

	var result sentimentResponse
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return "", 0, fmt.Errorf("%w: %s", ErrInvalidSentimentResponse, err)
	}

	// Validate sentiment value.
	switch result.Sentiment {
	case "positive", "negative", "neutral":
		// valid
	default:
		return "", 0, fmt.Errorf("%w: unknown sentiment %q", ErrInvalidSentimentResponse, result.Sentiment)
	}

	// Clamp score to [0, 1].
	if result.Score < 0 {
		result.Score = 0
	}

	if result.Score > 1 {
		result.Score = 1
	}

	return result.Sentiment, result.Score, nil
}
