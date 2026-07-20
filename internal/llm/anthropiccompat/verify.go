package anthropiccompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ListModels queries GET /v1/models (real Anthropic supports it; some compatibility
// endpoints do too). Endpoints without it return an error — callers fall back to
// Ping with an explicit model.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", apiVersion)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.errorFromResponse(resp)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("models response: %w", err)
	}
	out := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// Ping makes the cheapest possible messages call (1 output token, no image) to
// verify the base URL, API key, and model id all work together.
func (c *Client) Ping(ctx context.Context, model string) error {
	body := wireRequest{
		Model:     model,
		MaxTokens: 1,
		Messages: []wireMessage{{
			Role:    "user",
			Content: []wireContentBlock{{Type: "text", Text: "ping"}},
		}},
	}
	_, err := c.post(ctx, body)
	return err
}
