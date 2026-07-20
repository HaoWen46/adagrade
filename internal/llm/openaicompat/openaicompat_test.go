package openaicompat_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/openaicompat"
)

func testImage(t *testing.T) imaging.MaskedImage {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{200, 200, 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	m, err := imaging.LoadMasked("answers/1/masked/0-x.jpg", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var schema = []byte(`{"type":"object","properties":{"criteria":{"type":"array"}},"required":["criteria"]}`)

func gradeReq(t *testing.T) llm.Request {
	return llm.Request{
		System:      "sys prompt",
		Prompt:      "grade this",
		Images:      []imaging.ProviderImage{testImage(t)},
		Schema:      schema,
		Temperature: 0,
	}
}

func toolResponse(argsJSON string) string {
	b, _ := json.Marshal(map[string]any{
		"model": "test/model-1",
		"choices": []map[string]any{{
			"message": map[string]any{
				"content": nil,
				"tool_calls": []map[string]any{{
					"function": map[string]any{"name": "submit_grade", "arguments": argsJSON},
				}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": 42, "completion_tokens": 7},
	})
	return string(b)
}

func TestGradeRequestShapeAndToolCall(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-or-key" {
			t.Errorf("auth header: %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_, _ = w.Write([]byte(toolResponse(`{"criteria":[]}`)))
	}))
	defer srv.Close()

	c := openaicompat.New("openrouter", srv.URL, "sk-or-key")
	res, err := c.Grade(context.Background(), "google/gemini-2.5-flash", gradeReq(t))
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}

	if captured["model"] != "google/gemini-2.5-flash" {
		t.Errorf("model: %v", captured["model"])
	}
	msgs := captured["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages: %v", msgs)
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	img := parts[0].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("first part should be the image: %v", img)
	}
	url := img["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Errorf("image url prefix: %.40s", url)
	}
	if _, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/jpeg;base64,")); err != nil {
		t.Errorf("image payload not std base64: %v", err)
	}
	if parts[1].(map[string]any)["text"] != "grade this" {
		t.Errorf("text part: %v", parts[1])
	}
	tc := captured["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["function"].(map[string]any)["name"] != "submit_grade" {
		t.Errorf("tool_choice: %v", tc)
	}

	if string(res.JSON) != `{"criteria":[]}` || res.Model != "test/model-1" ||
		res.InputTokens != 42 || res.OutputTokens != 7 {
		t.Errorf("result: %+v", res)
	}
}

func TestGradeToolNameOverride(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		// Echo the caller's tool name back so the tool-call match path is exercised.
		b, _ := json.Marshal(map[string]any{
			"model": "test/model-1",
			"choices": []map[string]any{{
				"message": map[string]any{
					"content": nil,
					"tool_calls": []map[string]any{{
						"function": map[string]any{"name": "submit_identity", "arguments": `{"student_id":"b01"}`},
					}},
				},
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	req := gradeReq(t)
	req.ToolName = "submit_identity"
	c := openaicompat.New("openrouter", srv.URL, "sk-or-key")
	res, err := c.Grade(context.Background(), "cheap/vlm", req)
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}

	tc := captured["tool_choice"].(map[string]any)
	if tc["function"].(map[string]any)["name"] != "submit_identity" {
		t.Errorf("tool_choice name: %v, want submit_identity", tc)
	}
	tools := captured["tools"].([]any)
	if tools[0].(map[string]any)["function"].(map[string]any)["name"] != "submit_identity" {
		t.Errorf("tool name: %v, want submit_identity", tools[0])
	}
	if string(res.JSON) != `{"student_id":"b01"}` {
		t.Errorf("result JSON: %s", res.JSON)
	}
}

func TestGradeToolNameDefaultsToSubmitGrade(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_, _ = w.Write([]byte(toolResponse(`{"criteria":[]}`)))
	}))
	defer srv.Close()

	c := openaicompat.New("openrouter", srv.URL, "sk-or-key")
	if _, err := c.Grade(context.Background(), "m", gradeReq(t)); err != nil {
		t.Fatalf("Grade: %v", err)
	}
	tc := captured["tool_choice"].(map[string]any)
	if tc["function"].(map[string]any)["name"] != "submit_grade" {
		t.Errorf("empty ToolName should default to submit_grade, got %v", tc)
	}
}

func TestGradeReasoningLevelMapping(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_, _ = w.Write([]byte(toolResponse(`{"criteria":[]}`)))
	}))
	defer srv.Close()
	c := openaicompat.New("x", srv.URL, "k")

	cases := []struct {
		level string
		want  map[string]any // nil = reasoning field absent
	}{
		{"", nil},
		{"off", map[string]any{"enabled": false}},
		{"low", map[string]any{"effort": "low"}},
		{"high", map[string]any{"effort": "high"}},
	}
	for _, tc := range cases {
		req := gradeReq(t)
		req.ReasoningLevel = tc.level
		if _, err := c.Grade(context.Background(), "m", req); err != nil {
			t.Fatalf("level %q: %v", tc.level, err)
		}
		got, present := captured["reasoning"]
		if tc.want == nil {
			if present {
				t.Errorf("level %q: reasoning should be absent, got %v", tc.level, got)
			}
			continue
		}
		gm, _ := got.(map[string]any)
		for k, v := range tc.want {
			if gm[k] != v {
				t.Errorf("level %q: reasoning[%s] = %v, want %v", tc.level, k, gm[k], v)
			}
		}
	}
}

func TestGradeRetriesWithoutToolsOn400(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if calls == 1 {
			if body["tools"] == nil {
				t.Error("first call should carry tools")
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"this model does not support function calling"}}`))
			return
		}
		if body["tools"] != nil {
			t.Error("retry must not carry tools")
		}
		prompt := body["messages"].([]any)[1].(map[string]any)["content"].([]any)[1].(map[string]any)["text"].(string)
		if !strings.Contains(prompt, `"criteria"`) {
			t.Error("retry prompt should embed the schema")
		}
		b, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{
				"content": "```json\n{\"criteria\":[1]}\n```",
			}}},
		})
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	c := openaicompat.New("x", srv.URL, "k")
	res, err := c.Grade(context.Background(), "m", gradeReq(t))
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if string(res.JSON) != `{"criteria":[1]}` {
		t.Errorf("extracted JSON: %s", res.JSON)
	}
}

func TestGradeRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := openaicompat.New("x", srv.URL, "k")
	_, err := c.Grade(context.Background(), "m", gradeReq(t))
	var rl *llm.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 17*time.Second {
		t.Fatalf("want RateLimitError 17s, got %v", err)
	}
}

func TestGradeErrorSurfacedWithoutRequestContents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"auth","message":"bad key"}}`))
	}))
	defer srv.Close()

	c := openaicompat.New("x", srv.URL, "k")
	_, err := c.Grade(context.Background(), "m", gradeReq(t))
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("error should surface server message: %v", err)
	}
	if strings.Contains(err.Error(), "grade this") {
		t.Error("error must not contain request contents")
	}
}

func TestGradeNoStructuredOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "no json here"}}},
		})
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	c := openaicompat.New("x", srv.URL, "k")
	_, err := c.Grade(context.Background(), "m", gradeReq(t))
	if !errors.Is(err, llm.ErrNoStructuredOutput) {
		t.Fatalf("want ErrNoStructuredOutput, got %v", err)
	}
}

func TestListModelsAndPing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"google/gemini-2.5-flash"},{"id":"qwen/qwen3-vl-plus"}]}`))
		case "/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["max_tokens"].(float64) != 1 {
				t.Errorf("ping should use max_tokens 1: %v", body["max_tokens"])
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"p"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := openaicompat.New("openrouter", srv.URL, "k")
	models, err := c.ListModels(context.Background())
	if err != nil || len(models) != 2 || models[0] != "google/gemini-2.5-flash" {
		t.Fatalf("models: %v err=%v", models, err)
	}
	if err := c.Ping(context.Background(), "any/model"); err != nil {
		t.Fatalf("ping: %v", err)
	}
}
