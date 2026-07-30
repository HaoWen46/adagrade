package transcribe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/openaicompat"
)

// Opt-in live test against the real OpenRouter API, following the repo's
// live_test.go convention. Costs a fraction of a cent per run.
//
//	OPENROUTER_API_KEY=... TRANSCRIBE_LIVE=1 go test ./internal/transcribe/ -run Live_OpenRouter -v
//
// It proves the whole loop on a real masked page: image -> blocks -> validated
// -> .tex -> compiled PDF. Gated behind TRANSCRIBE_LIVE so a plain `go test`
// never spends money.

func openRouterOrSkip(t *testing.T) *openaicompat.Client {
	t.Helper()
	if os.Getenv("TRANSCRIBE_LIVE") == "" {
		t.Skip("set TRANSCRIBE_LIVE=1 to run the paid OpenRouter transcription test")
	}
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY unset")
	}
	base := os.Getenv("OPENROUTER_BASE_URL")
	if base == "" {
		base = "https://openrouter.ai/api/v1"
	}
	return openaicompat.New("openrouter", base, key)
}

func liveModel() string {
	if m := os.Getenv("TRANSCRIBE_MODEL"); m != "" {
		return m
	}
	return "google/gemini-3.1-flash-lite"
}

// maskedDemoPage loads one already-masked answer page from the dev blob store.
// LoadMasked's key gate ("/masked/") is what makes this a MaskedImage; an
// unmasked page cannot be constructed this way, so the test cannot accidentally
// send identity to a provider.
func maskedDemoPage(t *testing.T, rel string) imaging.MaskedImage {
	t.Helper()
	p := filepath.Join("../../data/blobs", rel)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("demo blob missing (%s): %v", rel, err)
	}
	img, err := imaging.LoadMasked(rel, b)
	if err != nil {
		t.Fatalf("LoadMasked(%s): %v", rel, err)
	}
	return img
}

func TestLive_OpenRouter_TranscribeEmitCompile(t *testing.T) {
	client := openRouterOrSkip(t)
	page := maskedDemoPage(t, "answers/2/masked/0-12cf38e5.jpg")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	res, err := client.Grade(ctx, liveModel(), llm.Request{
		System:      SystemPrompt,
		Prompt:      UserPrompt(2),
		Images:      []imaging.ProviderImage{page},
		Schema:      BuildSchema(),
		Temperature: 0,
		ToolName:    ToolName,
	})
	if err != nil {
		t.Fatalf("provider call failed: %v", err)
	}
	t.Logf("model=%s in=%d out=%d", res.Model, res.InputTokens, res.OutputTokens)

	doc, confidence, err := ParseResponse(res.JSON)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if len(doc.Blocks) == 0 {
		t.Fatalf("no blocks returned (confidence=%s)", confidence)
	}

	// Shape only — never the content, which is student answer text.
	kinds := map[BlockKind]int{}
	for _, b := range doc.Blocks {
		kinds[b.Kind]++
	}
	t.Logf("confidence=%s blocks=%d kinds=%v", confidence, len(doc.Blocks), kinds)

	font, err := filepath.Abs("../../data/fonts/NotoSansTC-Regular.ttf")
	if err != nil {
		t.Fatal(err)
	}
	doc.Title = "Problem 2"
	tex, flags := EmitTeXWith(doc, Options{CJKFontFile: font})
	if len(flags) > 0 {
		t.Logf("demotion flags: %v", flags)
	}

	bin, cache := engineOrSkip(t)
	pdf, err := Compile(ctx, bin, cache, tex)
	if err != nil {
		t.Fatalf("model output did not compile: %v", err)
	}
	if !strings.HasPrefix(string(pdf[:4]), "%PDF") {
		t.Fatalf("not a PDF (%d bytes)", len(pdf))
	}

	// Write the artefacts out so a human can do the professor's own QA:
	// compare this PDF against the source crop.
	outDir := os.Getenv("TRANSCRIBE_OUT")
	if outDir == "" {
		return
	}
	_ = os.MkdirAll(outDir, 0o755)
	_ = os.WriteFile(filepath.Join(outDir, "sample.tex"), []byte(tex), 0o600)
	_ = os.WriteFile(filepath.Join(outDir, "sample.pdf"), pdf, 0o600)
	t.Logf("wrote %s/sample.{tex,pdf} (%d-byte PDF)", outDir, len(pdf))
}

// TestLive_OpenRouter_TraditionalChineseWithMath is the case the public
// benchmarks do not cover: zh-Hant prose interleaved with inline math and an
// indented pseudocode block. Printed, not handwritten — so it isolates the
// language/notation problem from the handwriting problem.
func TestLive_OpenRouter_TraditionalChineseWithMath(t *testing.T) {
	client := openRouterOrSkip(t)

	b, err := os.ReadFile("testdata/zh-math.jpg")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	page, err := imaging.LoadMasked("answers/fixture/masked/zh-math.jpg", b)
	if err != nil {
		t.Fatalf("LoadMasked: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	res, err := client.Grade(ctx, liveModel(), llm.Request{
		System: SystemPrompt, Prompt: UserPrompt(3),
		Images: []imaging.ProviderImage{page}, Schema: BuildSchema(),
		Temperature: 0, ToolName: ToolName,
	})
	if err != nil {
		t.Fatalf("provider call failed: %v", err)
	}
	doc, confidence, err := ParseResponse(res.JSON)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	t.Logf("model=%s in=%d out=%d confidence=%s blocks=%d", res.Model, res.InputTokens, res.OutputTokens, confidence, len(doc.Blocks))

	// This fixture is synthetic and contains no student PII, so asserting on
	// its content is safe — and it is the only way to measure fidelity.
	var code, prose int
	for _, blk := range doc.Blocks {
		switch blk.Kind {
		case BlockCode:
			code++
			if !strings.Contains(blk.Text, "    ") {
				t.Errorf("pseudocode lost its indentation: %q", blk.Text)
			}
		case BlockProse:
			prose++
		}
	}
	if code == 0 {
		t.Error("the pseudocode block was not recognised as code")
	}

	joined := docText(doc)
	for _, want := range []string{"動態規劃", "初始條件", "時間複雜度", "單調佇列"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Traditional Chinese phrase %q missing from transcription", want)
		}
	}
	// Simplified-character leakage is the documented zh-Hant failure mode.
	for _, bad := range []string{"动态规划", "复杂度", "初始条件", "队列"} {
		if strings.Contains(joined, bad) {
			t.Errorf("Simplified Chinese %q leaked into a Traditional Chinese transcription", bad)
		}
	}

	font, _ := filepath.Abs("../../data/fonts/NotoSansTC-Regular.ttf")
	doc.Title = "Problem 3"
	tex, flags := EmitTeXWith(doc, Options{CJKFontFile: font})
	t.Logf("flags=%v", flags)

	bin, cache := engineOrSkip(t)
	pdf, err := Compile(ctx, bin, cache, tex)
	if err != nil {
		t.Fatalf("zh-Hant transcription did not compile: %v", err)
	}
	if out := os.Getenv("TRANSCRIBE_OUT"); out != "" {
		_ = os.MkdirAll(out, 0o755)
		_ = os.WriteFile(filepath.Join(out, "zh.tex"), []byte(tex), 0o600)
		_ = os.WriteFile(filepath.Join(out, "zh.pdf"), pdf, 0o600)
		t.Logf("wrote %s/zh.{tex,pdf}", out)
	}
}

func docText(d Doc) string {
	var sb strings.Builder
	for _, b := range d.Blocks {
		sb.WriteString(b.Text)
		sb.WriteString("\n")
		for _, it := range b.Items {
			sb.WriteString(it)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
