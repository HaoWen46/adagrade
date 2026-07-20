package anthropiccompat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/llm"
)

// tinyJPEG produces a real (decodable) JPEG so the test exercises the exact
// bytes MaskedImage.JPEG() will hand to the wire encoder.
func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode tiny JPEG: %v", err)
	}
	return buf.Bytes()
}

func testMasked(t *testing.T) imaging.MaskedImage {
	t.Helper()
	mi, err := imaging.LoadMasked("answers/1/masked/0-abc.jpg", tinyJPEG(t))
	if err != nil {
		t.Fatalf("LoadMasked: %v", err)
	}
	return mi
}

var testSchema = []byte(`{"type":"object","properties":{"score":{"type":"number"}}}`)

// toolUseResponse is a canned happy-path Messages API response.
func toolUseResponse() string {
	return `{
		"id": "msg_01",
		"model": "some-model-20250101",
		"content": [
			{"type": "text", "text": "Grading now."},
			{"type": "tool_use", "id": "toolu_01", "name": "submit_grade", "input": {"score": 7}}
		],
		"usage": {"input_tokens": 123, "output_tokens": 45}
	}`
}

func newTestClient(srvURL string) *Client {
	return New("test", srvURL, "sk-test-key")
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return m
}

func TestGradeRequestShape(t *testing.T) {
	img := testMasked(t)
	var got *http.Request
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		body = decodeBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(toolUseResponse()))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	req := llm.Request{
		System: "You are a strict grader.",
		Prompt: "Grade this answer.",
		Images: []imaging.ProviderImage{img},
		Schema: testSchema,
	}
	if _, err := c.Grade(context.Background(), "some-model", req); err != nil {
		t.Fatalf("Grade: %v", err)
	}

	// Endpoint + headers.
	if got.URL.Path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", got.URL.Path)
	}
	if got.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.Method)
	}
	if k := got.Header.Get("x-api-key"); k != "sk-test-key" {
		t.Errorf("x-api-key = %q", k)
	}
	if v := got.Header.Get("anthropic-version"); v != "2023-06-01" {
		t.Errorf("anthropic-version = %q", v)
	}
	if ct := got.Header.Get("content-type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}

	// Top-level body fields.
	if body["model"] != "some-model" {
		t.Errorf("model = %v", body["model"])
	}
	if mt, _ := body["max_tokens"].(float64); mt != 4096 {
		t.Errorf("max_tokens = %v, want default 4096", body["max_tokens"])
	}
	if body["system"] != "You are a strict grader." {
		t.Errorf("system = %v", body["system"])
	}

	// Forced tool use.
	tc, _ := body["tool_choice"].(map[string]any)
	if tc == nil || tc["type"] != "tool" || tc["name"] != "submit_grade" {
		t.Errorf("tool_choice = %v, want {type:tool, name:submit_grade}", body["tool_choice"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want exactly one", body["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "submit_grade" {
		t.Errorf("tool name = %v", tool["name"])
	}
	wantSchema, _ := tool["input_schema"].(map[string]any)
	if wantSchema == nil || wantSchema["type"] != "object" {
		t.Errorf("input_schema = %v, want the request schema", tool["input_schema"])
	}

	// One user message: image block(s) first, text block last.
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want exactly one", body["messages"])
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role = %v", msg["role"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content has %d blocks, want 2 (image, text)", len(content))
	}
	imgBlock := content[0].(map[string]any)
	if imgBlock["type"] != "image" {
		t.Errorf("first block type = %v, want image", imgBlock["type"])
	}
	src := imgBlock["source"].(map[string]any)
	if src["type"] != "base64" {
		t.Errorf("source.type = %v", src["type"])
	}
	if src["media_type"] != "image/jpeg" {
		t.Errorf("source.media_type = %v", src["media_type"])
	}
	if src["data"] != base64.StdEncoding.EncodeToString(img.JPEG()) {
		t.Errorf("source.data is not the std-base64 of MaskedImage.JPEG()")
	}
	txtBlock := content[len(content)-1].(map[string]any)
	if txtBlock["type"] != "text" || txtBlock["text"] != "Grade this answer." {
		t.Errorf("last block = %v, want text block with the prompt", txtBlock)
	}
}

func TestGradeToolNameOverride(t *testing.T) {
	img := testMasked(t)
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","content":[{"type":"tool_use","id":"t","name":"submit_identity","input":{"student_id":"b01"}}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	req := llm.Request{
		Prompt:   "Identify.",
		Images:   []imaging.ProviderImage{img},
		Schema:   testSchema,
		ToolName: "submit_identity",
	}
	if _, err := newTestClient(srv.URL).Grade(context.Background(), "m", req); err != nil {
		t.Fatalf("Grade: %v", err)
	}
	tc, _ := body["tool_choice"].(map[string]any)
	if tc == nil || tc["name"] != "submit_identity" {
		t.Errorf("tool_choice = %v, want name submit_identity", body["tool_choice"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "submit_identity" {
		t.Errorf("tools = %v, want one named submit_identity", body["tools"])
	}
}

func TestGradeToolNameDefaultsToSubmitGrade(t *testing.T) {
	img := testMasked(t)
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(toolUseResponse()))
	}))
	defer srv.Close()

	req := llm.Request{Prompt: "Grade.", Images: []imaging.ProviderImage{img}, Schema: testSchema}
	if _, err := newTestClient(srv.URL).Grade(context.Background(), "m", req); err != nil {
		t.Fatalf("Grade: %v", err)
	}
	tc, _ := body["tool_choice"].(map[string]any)
	if tc == nil || tc["name"] != "submit_grade" {
		t.Errorf("empty ToolName should default to submit_grade, got %v", body["tool_choice"])
	}
}

func TestGradeToolUseResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(toolUseResponse()))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	res, err := c.Grade(context.Background(), "some-model", llm.Request{
		Prompt: "Grade.", Images: []imaging.ProviderImage{testMasked(t)}, Schema: testSchema,
	})
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(res.JSON, &out); err != nil {
		t.Fatalf("Result.JSON not valid JSON: %v (%s)", err, res.JSON)
	}
	if out["score"] != float64(7) {
		t.Errorf("Result.JSON = %s, want score 7", res.JSON)
	}
	if res.RawText != "Grading now." {
		t.Errorf("RawText = %q", res.RawText)
	}
	if res.Model != "some-model-20250101" {
		t.Errorf("Model = %q", res.Model)
	}
	if res.InputTokens != 123 || res.OutputTokens != 45 {
		t.Errorf("tokens = %d/%d, want 123/45", res.InputTokens, res.OutputTokens)
	}
}

func TestGradeTextJSONFallbackWhenNoToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "m",
			"content": [{"type": "text", "text": "Here you go: {\"score\": 3} hope that helps"}],
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	res, err := c.Grade(context.Background(), "m", llm.Request{Prompt: "p", Schema: testSchema})
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if string(res.JSON) != `{"score": 3}` {
		t.Errorf("Result.JSON = %q, want extracted {\"score\": 3}", res.JSON)
	}
}

func TestGradeRetriesWithoutToolsOn400MentioningTools(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodies = append(bodies, decodeBody(t, r))
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"tool_choice is not supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"model": "m",
			"content": [{"type": "text", "text": "` + "```json\\n{\\\"score\\\": 5}\\n```" + `"}],
			"usage": {"input_tokens": 20, "output_tokens": 9}
		}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	res, err := c.Grade(context.Background(), "m", llm.Request{Prompt: "Grade it.", Schema: testSchema})
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("server saw %d requests, want 2 (retry without tools)", len(bodies))
	}

	second := bodies[1]
	if _, has := second["tools"]; has {
		t.Errorf("second request still has tools field: %v", second["tools"])
	}
	if _, has := second["tool_choice"]; has {
		t.Errorf("second request still has tool_choice field: %v", second["tool_choice"])
	}
	msg := second["messages"].([]any)[0].(map[string]any)
	content := msg["content"].([]any)
	txt := content[len(content)-1].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "Grade it.") {
		t.Errorf("retry prompt lost the original prompt: %q", txt)
	}
	if !strings.Contains(txt, string(testSchema)) {
		t.Errorf("retry prompt does not embed the schema: %q", txt)
	}
	if !strings.Contains(txt, "Respond with ONLY a JSON object") {
		t.Errorf("retry prompt missing JSON-only instruction: %q", txt)
	}

	// Fenced ```json block extracted.
	var out map[string]any
	if err := json.Unmarshal(res.JSON, &out); err != nil {
		t.Fatalf("Result.JSON invalid: %v (%s)", err, res.JSON)
	}
	if out["score"] != float64(5) {
		t.Errorf("Result.JSON = %s, want score 5", res.JSON)
	}
}

func TestGrade400NotMentioningToolsIsNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"max_tokens too large"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.Grade(context.Background(), "m", llm.Request{Prompt: "p", Schema: testSchema})
	if err == nil {
		t.Fatal("want error")
	}
	if calls != 1 {
		t.Errorf("server saw %d calls, want 1 (no retry)", calls)
	}
	if !strings.Contains(err.Error(), "max_tokens too large") {
		t.Errorf("error should surface the API message: %v", err)
	}
}

func TestGradeRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.Grade(context.Background(), "m", llm.Request{Prompt: "p"})
	var rle *llm.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v (%T), want *llm.RateLimitError", err, err)
	}
	if rle.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %s, want 7s", rle.RetryAfter)
	}
}

func TestGradeRateLimitedNoHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.Grade(context.Background(), "m", llm.Request{Prompt: "p"})
	var rle *llm.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v (%T), want *llm.RateLimitError", err, err)
	}
	if rle.RetryAfter != 0 {
		t.Errorf("RetryAfter = %s, want 0", rle.RetryAfter)
	}
}

func TestGradeServerErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"upstream exploded"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.Grade(context.Background(), "m", llm.Request{Prompt: "p"})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"500", "api_error", "upstream exploded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestGradeNoStructuredOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "m",
			"content": [{"type": "text", "text": "I cannot grade this."}],
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.Grade(context.Background(), "m", llm.Request{Prompt: "p", Schema: testSchema})
	if !errors.Is(err, llm.ErrNoStructuredOutput) {
		t.Fatalf("err = %v, want ErrNoStructuredOutput", err)
	}
}

func TestGradeNoSchemaReturnsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		if _, has := body["tools"]; has {
			t.Errorf("no-schema request must not send tools: %v", body["tools"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "m",
			"content": [{"type": "text", "text": "plain answer"}],
			"usage": {"input_tokens": 1, "output_tokens": 2}
		}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	res, err := c.Grade(context.Background(), "m", llm.Request{Prompt: "p"})
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if res.RawText != "plain answer" {
		t.Errorf("RawText = %q", res.RawText)
	}
	if res.JSON != nil {
		t.Errorf("JSON = %q, want nil when no schema requested", res.JSON)
	}
}

func TestGradeContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	c := newTestClient(srv.URL)
	_, err := c.Grade(ctx, "m", llm.Request{Prompt: "p"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestName(t *testing.T) {
	if got := New("deepseek", "https://example.test", "k").Name(); got != "deepseek" {
		t.Errorf("Name() = %q", got)
	}
}
