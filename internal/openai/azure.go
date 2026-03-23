package openai

import (
	"net/http"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/amrhym/hivecfm-hub/internal/models"
)

// azureTransport injects the api-key header and api-version query parameter for Azure OpenAI.
type azureTransport struct {
	apiKey     string
	apiVersion string
	base       http.RoundTripper
}

func (t *azureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("api-key", t.apiKey)
	// Remove Authorization header (OpenAI SDK sets Bearer token)
	req.Header.Del("Authorization")
	// Add api-version query parameter
	q := req.URL.Query()
	q.Set("api-version", t.apiVersion)
	req.URL.RawQuery = q.Encode()
	return t.base.RoundTrip(req)
}

// NewAzureClient creates an OpenAI embeddings client that targets Azure OpenAI.
// endpoint is the Azure OpenAI resource endpoint (e.g. https://myresource.openai.azure.com).
// apiKey is the Azure API key.
// model is the deployment name (e.g. text-embedding-3-small).
// normalize enables L2 normalization of embedding vectors.
func NewAzureClient(endpoint, apiKey, model string, normalize bool) *Client {
	transport := &azureTransport{
		apiKey:     apiKey,
		apiVersion: "2024-06-01",
		base:       http.DefaultTransport,
	}

	baseURL := endpoint + "/openai/deployments/" + model

	sdk := openaisdk.NewClient(
		option.WithAPIKey("azure"), // placeholder, overridden by transport
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)

	return &Client{
		sdk:        sdk,
		dimensions: models.EmbeddingVectorDimensions,
		model:      model,
		normalize:  normalize,
	}
}

// NewAzureChatClient creates a ChatClient for sentiment analysis targeting Azure OpenAI.
// endpoint is the Azure OpenAI resource endpoint.
// apiKey is the Azure API key.
// model is the deployment name for chat completions (e.g. gpt-4o-mini).
func NewAzureChatClient(endpoint, apiKey, model string) *ChatClient {
	transport := &azureTransport{
		apiKey:     apiKey,
		apiVersion: "2024-06-01",
		base:       http.DefaultTransport,
	}

	baseURL := endpoint + "/openai/deployments/" + model

	sdk := openaisdk.NewClient(
		option.WithAPIKey("azure"),
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)

	return &ChatClient{
		sdk:   sdk,
		model: model,
	}
}
