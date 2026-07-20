// Package anthropiccompat implements the llm.Provider seam over the Anthropic
// Messages API wire shape. The base URL is configurable because the same shape
// is spoken by DeepSeek (https://api.deepseek.com/anthropic) and Qwen
// (https://dashscope-intl.aliyuncs.com/apps/anthropic) compatibility endpoints
// as well as real Anthropic (docs/DECISIONS.md D11).
//
// Structured output uses forced tool use ("submit_grade"). Compatibility
// endpoints that reject tools with an HTTP 400 mentioning "tool" get one
// retry without tools, asking for raw JSON in the text instead.
package anthropiccompat

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
	apiVersion       = "2023-06-01"
	defaultMaxTokens = 4096
	defaultTimeout   = 120 * time.Second
	gradeToolName    = "submit_grade"
	maxErrorBody     = 8 << 10 // read at most 8KB of an error body
)

// Client speaks the Anthropic Messages API against a configurable base URL.
type Client struct {
	// HTTPClient may be replaced (e.g. for custom transports). New sets a
	// 120s-timeout default; per-call deadlines come from the context.
	HTTPClient *http.Client

	name    string
	baseURL string
	apiKey  string
}

// New returns a Client named name, posting to {baseURL}/v1/messages with apiKey.
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

type wireImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type wireContentBlock struct {
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *wireImageSource `json:"source,omitempty"`
}

type wireMessage struct {
	Role    string             `json:"role"`
	Content []wireContentBlock `json:"content"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type wireRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature"`
	System      string          `json:"system,omitempty"`
	Messages    []wireMessage   `json:"messages"`
	Tools       []wireTool      `json:"tools,omitempty"`
	ToolChoice  *wireToolChoice `json:"tool_choice,omitempty"`
}

type wireResponseBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Input json.RawMessage `json:"input"` // tool_use blocks
}

type wireResponse struct {
	Model   string              `json:"model"`
	Content []wireResponseBlock `json:"content"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// apiError is a non-2xx reply. It never carries request contents — only what
// the server sent back (status, error type, message).
type apiError struct {
	status  int
	errType string
	message string
	rawBody string // for the "mentions tool" 400 sniff; capped at maxErrorBody
}

func (e *apiError) Error() string {
	if e.errType == "" && e.message == "" {
		return fmt.Sprintf("anthropiccompat: HTTP %d", e.status)
	}
	return fmt.Sprintf("anthropiccompat: HTTP %d: %s: %s", e.status, e.errType, e.message)
}

// mentionsTools reports whether this is the "compat endpoint rejects tools"
// 400 that warrants a single no-tools retry.
func (e *apiError) mentionsTools() bool {
	return e.status == http.StatusBadRequest && strings.Contains(strings.ToLower(e.rawBody), "tool")
}

// ---- Grade ----

// Grade implements llm.Provider.
func (c *Client) Grade(ctx context.Context, model string, req llm.Request) (llm.Result, error) {
	withTools := req.Schema != nil

	resp, err := c.post(ctx, buildWireRequest(model, req, withTools))
	if err != nil {
		var ae *apiError
		if withTools && errors.As(err, &ae) && ae.mentionsTools() {
			return c.gradeWithoutTools(ctx, model, req)
		}
		return llm.Result{}, err
	}
	return parseResult(resp, withTools)
}

// gradeWithoutTools is the graceful-degradation path: the endpoint rejected
// tools, so re-ask once with the schema inlined in the prompt and extract
// JSON from the text reply.
func (c *Client) gradeWithoutTools(ctx context.Context, model string, req llm.Request) (llm.Result, error) {
	req.Prompt += "\n\nRespond with ONLY a JSON object matching this schema (no markdown fences):\n" + string(req.Schema)
	resp, err := c.post(ctx, buildWireRequest(model, req, false))
	if err != nil {
		return llm.Result{}, err
	}

	res := baseResult(resp)
	extracted, ok := extractJSON(stripJSONFences(res.RawText))
	if !ok {
		return llm.Result{}, fmt.Errorf("anthropiccompat: no-tools retry reply had no JSON: %w", llm.ErrNoStructuredOutput)
	}
	res.JSON = extracted
	return res, nil
}

func buildWireRequest(model string, req llm.Request, withTools bool) wireRequest {
	content := make([]wireContentBlock, 0, len(req.Images)+1)
	for _, img := range req.Images {
		content = append(content, wireContentBlock{
			Type: "image",
			Source: &wireImageSource{
				Type:      "base64",
				MediaType: "image/jpeg",
				Data:      base64.StdEncoding.EncodeToString(img.JPEG()),
			},
		})
	}
	content = append(content, wireContentBlock{Type: "text", Text: req.Prompt})

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}

	w := wireRequest{
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		System:      req.System,
		Messages:    []wireMessage{{Role: "user", Content: content}},
	}
	if withTools {
		name := req.ToolName
		if name == "" {
			name = gradeToolName
		}
		w.Tools = []wireTool{{
			Name:        name,
			Description: "Submit the structured result.",
			InputSchema: json.RawMessage(req.Schema),
		}}
		w.ToolChoice = &wireToolChoice{Type: "tool", Name: name}
	}
	return w
}

// post sends one Messages API call and decodes the success body. Non-2xx
// replies come back as *apiError (or *llm.RateLimitError for 429).
func (c *Client) post(ctx context.Context, body wireRequest) (*wireResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropiccompat: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("anthropiccompat: build request: %w", err)
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)
	httpReq.Header.Set("content-type", "application/json")

	httpResp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropiccompat: %s: %w", c.name, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return nil, c.errorFromResponse(httpResp)
	}

	var resp wireResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("anthropiccompat: decode response: %w", err)
	}
	return &resp, nil
}

// errorFromResponse turns a non-2xx reply into an error. The request contents
// are NEVER included — only server-provided status/type/message.
func (c *Client) errorFromResponse(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))

	if resp.StatusCode == http.StatusTooManyRequests {
		return &llm.RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}

	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &parsed) // best-effort; plain-text bodies still yield the status
	return &apiError{
		status:  resp.StatusCode,
		errType: parsed.Error.Type,
		message: parsed.Error.Message,
		rawBody: string(raw),
	}
}

// parseRetryAfter handles both forms RFC 9110 allows: delta-seconds and an
// HTTP date. Anything unparseable (or in the past) is zero.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}

// baseResult fills the fields every reply has: concatenated text, model, usage.
func baseResult(resp *wireResponse) llm.Result {
	var text strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	return llm.Result{
		RawText:      text.String(),
		Model:        resp.Model,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
	}
}

// parseResult handles the primary (tools sent, or no schema at all) reply:
// prefer the forced tool_use input; fall back to JSON embedded in the text;
// error with ErrNoStructuredOutput when a schema was requested but neither
// yields JSON.
func parseResult(resp *wireResponse, schemaRequested bool) (llm.Result, error) {
	res := baseResult(resp)

	for _, b := range resp.Content {
		if b.Type == "tool_use" && len(b.Input) > 0 {
			res.JSON = append([]byte(nil), b.Input...)
			return res, nil
		}
	}

	if !schemaRequested {
		return res, nil
	}
	extracted, ok := extractJSON(res.RawText)
	if !ok {
		return llm.Result{}, fmt.Errorf("anthropiccompat: no tool_use block and no JSON in text: %w", llm.ErrNoStructuredOutput)
	}
	res.JSON = extracted
	return res, nil
}

// extractJSON pulls the first-`{`-to-last-`}` span out of s if it is valid JSON.
func extractJSON(s string) ([]byte, bool) {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return nil, false
	}
	candidate := []byte(s[start : end+1])
	if !json.Valid(candidate) {
		return nil, false
	}
	return candidate, true
}

// stripJSONFences unwraps a ```json ... ``` (or bare ```) fenced block so that
// prose outside the fence cannot confuse first-{/last-} extraction.
func stripJSONFences(s string) string {
	open := strings.Index(s, "```")
	if open < 0 {
		return s
	}
	rest := s[open+3:]
	// Drop the optional language tag ("json") up to the end of that line.
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	} else {
		rest = strings.TrimPrefix(rest, "json")
	}
	if end := strings.Index(rest, "```"); end >= 0 {
		return rest[:end]
	}
	return rest
}
