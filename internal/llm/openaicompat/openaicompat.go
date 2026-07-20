// Package openaicompat implements the llm.Provider seam over the OpenAI Chat
// Completions wire shape. The base URL is configurable because the same shape is
// spoken by OpenRouter (https://openrouter.ai/api/v1 — hundreds of models behind one
// key, model ids namespaced like "google/gemini-2.5-flash"), OpenAI itself, and many
// self-hosted gateways (docs/DECISIONS.md D11 v1).
//
// Structured output uses a forced function tool call ("submit_grade"). Endpoints or
// models that reject tools with an HTTP 400 mentioning tool/function get one retry
// without tools, asking for raw JSON in the text instead.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HaoWen46/adagrade/internal/llm"
)

const (
	defaultMaxTokens = 4096
	defaultTimeout   = 120 * time.Second
	gradeToolName    = "submit_grade"
	maxErrorBody     = 8 << 10
)

// Client speaks the OpenAI Chat Completions API against a configurable base URL.
type Client struct {
	// HTTPClient may be replaced; New sets a 120s-timeout default.
	HTTPClient *http.Client

	name    string
	baseURL string
	apiKey  string
}

// New returns a Client named name, posting to {baseURL}/chat/completions with apiKey.
func New(name, baseURL, apiKey string) *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: defaultTimeout},
		name:       name,
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
	}
}

// Name implements llm.Provider.
func (c *Client) Name() string { return c.name }

// ---- wire types ----

type wireImageURL struct {
	URL string `json:"url"`
}

type wireContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

// wireMessage's Content is a string for the system message and []wireContentPart for
// the multimodal user message.
type wireMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireToolChoice struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// wireReasoning is OpenRouter's normalized reasoning control (effort levels work
// across vendors; enabled=false disables thinking where the model allows it).
type wireReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type wireRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature"`
	Messages    []wireMessage   `json:"messages"`
	Tools       []wireTool      `json:"tools,omitempty"`
	ToolChoice  *wireToolChoice `json:"tool_choice,omitempty"`
	Reasoning   *wireReasoning  `json:"reasoning,omitempty"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   json.RawMessage `json:"content"` // string, null, or (rare) parts array
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"` // JSON as a string
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// apiError is a non-2xx reply; it never carries request contents.
type apiError struct {
	status  int
	errType string
	message string
	rawBody string
}

func (e *apiError) Error() string {
	if e.errType == "" && e.message == "" {
		return fmt.Sprintf("openaicompat: HTTP %d", e.status)
	}
	return fmt.Sprintf("openaicompat: HTTP %d: %s: %s", e.status, e.errType, e.message)
}

// mentionsTools reports the "endpoint/model rejects tools" 400 that warrants a
// single no-tools retry.
func (e *apiError) mentionsTools() bool {
	body := strings.ToLower(e.rawBody)
	return e.status == http.StatusBadRequest &&
		(strings.Contains(body, "tool") || strings.Contains(body, "function"))
}

// ---- Grade ----

// Grade implements llm.Provider.
func (c *Client) Grade(ctx context.Context, model string, req llm.Request) (llm.Result, error) {
	resp, err := c.post(ctx, buildWireRequest(model, req, req.Schema != nil))
	var ae *apiError
	if errors.As(err, &ae) && req.Schema != nil && ae.mentionsTools() {
		return c.gradeWithoutTools(ctx, model, req)
	}
	if err != nil {
		return llm.Result{}, err
	}
	return parseResult(resp, req.Schema != nil, toolName(req))
}

// toolName resolves the forced-tool name for req, defaulting to the grading tool
// when the caller leaves ToolName empty.
func toolName(req llm.Request) string {
	if req.ToolName != "" {
		return req.ToolName
	}
	return gradeToolName
}

func (c *Client) gradeWithoutTools(ctx context.Context, model string, req llm.Request) (llm.Result, error) {
	plain := req
	plain.Prompt = req.Prompt +
		"\n\nRespond with ONLY a JSON object matching this schema (no markdown fences):\n" +
		string(req.Schema)
	resp, err := c.post(ctx, buildWireRequest(model, plain, false))
	if err != nil {
		return llm.Result{}, err
	}
	return parseResult(resp, true, toolName(req))
}

func buildWireRequest(model string, req llm.Request, withTools bool) wireRequest {
	parts := make([]wireContentPart, 0, len(req.Images)+1)
	for _, img := range req.Images {
		parts = append(parts, wireContentPart{
			Type: "image_url",
			ImageURL: &wireImageURL{
				URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(img.JPEG()),
			},
		})
	}
	parts = append(parts, wireContentPart{Type: "text", Text: req.Prompt})

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	out := wireRequest{
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		Reasoning:   reasoningParam(req.ReasoningLevel),
	}
	if req.System != "" {
		out.Messages = append(out.Messages, wireMessage{Role: "system", Content: req.System})
	}
	out.Messages = append(out.Messages, wireMessage{Role: "user", Content: parts})

	if withTools {
		name := toolName(req)
		out.Tools = []wireTool{{
			Type: "function",
			Function: wireFunction{
				Name:        name,
				Description: "Submit the structured result.",
				Parameters:  json.RawMessage(req.Schema),
			},
		}}
		tc := &wireToolChoice{Type: "function"}
		tc.Function.Name = name
		out.ToolChoice = tc
	}
	return out
}

// reasoningParam maps the method's abstract reasoning_level onto OpenRouter's
// normalized reasoning control. "" means "model default" (send nothing); "off"
// requests thinking disabled (models with mandatory reasoning run their minimum
// anyway); low/medium/high are effort levels normalized across vendors.
func reasoningParam(level string) *wireReasoning {
	switch level {
	case "off":
		f := false
		return &wireReasoning{Enabled: &f}
	case "low", "medium", "high":
		return &wireReasoning{Effort: level}
	default:
		return nil
	}
}

func (c *Client) post(ctx context.Context, body wireRequest) (*wireResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	// OpenRouter attribution headers; other endpoints ignore them.
	httpReq.Header.Set("X-Title", "ADA-Marker")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.errorFromResponse(resp)
	}

	var out wireResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("openaicompat: decode response: %w", err)
	}
	return &out, nil
}

func (c *Client) errorFromResponse(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if resp.StatusCode == http.StatusTooManyRequests {
		return &llm.RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	ae := &apiError{status: resp.StatusCode, rawBody: string(raw)}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil {
		ae.errType = body.Error.Type
		ae.message = body.Error.Message
	}
	return ae
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

func parseResult(resp *wireResponse, schemaRequested bool, wantTool string) (llm.Result, error) {
	out := llm.Result{
		Model:        resp.Model,
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}
	if len(resp.Choices) == 0 {
		return out, fmt.Errorf("openaicompat: response has no choices")
	}
	msg := resp.Choices[0].Message

	out.RawText = contentText(msg.Content)
	for _, tc := range msg.ToolCalls {
		if tc.Function.Name == wantTool || len(msg.ToolCalls) == 1 {
			args := []byte(tc.Function.Arguments)
			if json.Valid(args) {
				out.JSON = args
				return out, nil
			}
		}
	}
	if schemaRequested {
		if extracted, ok := extractJSON(stripJSONFences(out.RawText)); ok {
			out.JSON = extracted
			return out, nil
		}
		return out, fmt.Errorf("openaicompat: %w", llm.ErrNoStructuredOutput)
	}
	return out, nil
}

// contentText renders the assistant content, which is usually a JSON string but may
// be null or (on some gateways) an array of text parts.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

func extractJSON(s string) ([]byte, bool) {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	candidate := []byte(s[start : end+1])
	if !json.Valid(candidate) {
		return nil, false
	}
	return candidate, true
}

func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}
