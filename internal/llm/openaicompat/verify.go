package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ListModels queries GET {base}/models (OpenAI shape; OpenRouter supports it and
// returns its full catalog).
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
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

// Ping makes the cheapest possible completion (1 output token, no image) to verify
// base URL + key + model together.
func (c *Client) Ping(ctx context.Context, model string) error {
	body := wireRequest{
		Model:     model,
		MaxTokens: 1,
		Messages:  []wireMessage{{Role: "user", Content: "ping"}},
	}
	_, err := c.post(ctx, body)
	return err
}
