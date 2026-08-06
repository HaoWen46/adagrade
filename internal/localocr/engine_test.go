package localocr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/HaoWen46/adagrade/internal/imaging"
)

func TestNew_MissingModelNamesPath(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys.txt")
	if err := os.WriteFile(keys, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingModel := filepath.Join(dir, "nope-model.onnx")
	_, err := New(Config{
		ModelPath:          missingModel,
		KeysPath:           keys,
		ONNXRuntimeLibPath: filepath.Join(dir, "libonnxruntime.dylib"),
	})
	if err == nil {
		t.Fatal("want error for missing model file, got nil")
	}
	if !strings.Contains(err.Error(), missingModel) {
		t.Errorf("error should name the missing model path %q; got: %v", missingModel, err)
	}
	if !strings.Contains(err.Error(), "localocr:") {
		t.Errorf("error should carry localocr: prefix; got: %v", err)
	}
}

func TestNew_MissingKeysNamesPath(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "model.onnx")
	if err := os.WriteFile(model, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingKeys := filepath.Join(dir, "nope-keys.txt")
	_, err := New(Config{
		ModelPath:          model,
		KeysPath:           missingKeys,
		ONNXRuntimeLibPath: filepath.Join(dir, "libonnxruntime.dylib"),
	})
	if err == nil {
		t.Fatal("want error for missing keys file, got nil")
	}
	if !strings.Contains(err.Error(), missingKeys) {
		t.Errorf("error should name the missing keys path %q; got: %v", missingKeys, err)
	}
}

func TestNew_MissingRuntimeNamesPath(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "model.onnx")
	keys := filepath.Join(dir, "keys.txt")
	if err := os.WriteFile(model, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keys, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingLib := filepath.Join(dir, "nope-libonnxruntime.dylib")
	_, err := New(Config{
		ModelPath:          model,
		KeysPath:           keys,
		ONNXRuntimeLibPath: missingLib,
	})
	if err == nil {
		t.Fatal("want error for missing runtime lib, got nil")
	}
	if !strings.Contains(err.Error(), missingLib) {
		t.Errorf("error should name the missing runtime path %q; got: %v", missingLib, err)
	}
}

func TestNew_EmptyConfigFieldsRejected(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("want error for empty config, got nil")
	}
	if !strings.Contains(err.Error(), "localocr:") {
		t.Errorf("error should carry localocr: prefix; got: %v", err)
	}
}

func TestLoadKeys_TrimsAndCounts(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys.txt")
	// PP-OCR dict: one glyph per line, no header. A trailing newline must not
	// create a phantom empty entry.
	if err := os.WriteFile(keys, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 keys, got %d: %q", len(got), string(got))
	}
	if got[0] != 'a' || got[2] != 'c' {
		t.Errorf("keys mismatch: %q", string(got))
	}
}

func TestLoadKeys_MissingFileNamesPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.txt")
	_, err := loadKeys(missing)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error should name path %q; got %v", missing, err)
	}
}

// TestValidateClassCount is Finding 2: when the model's output class count is
// statically known, it must be exactly len(keys)+1 (blank) or len(keys)+2
// (blank + trailing space) — any other count means the model and the keys
// file were not exported together, and proceeding anyway would silently mis-
// decode every line (classToRune clamping out-of-range classes) instead of
// failing loudly at startup.
func TestValidateClassCount(t *testing.T) {
	const model, keys = "model.onnx", "keys.txt"
	cases := []struct {
		name              string
		outClasses        int
		numKeys           int
		wantTrailingSpace bool
		wantErr           bool
	}{
		{"blank-only matches M+1", 10, 9, false, false},
		{"trailing-space matches M+2", 11, 9, true, false},
		{"dynamic dim (<=0) unknown, no check yet", 0, 9, false, false},
		{"negative dim treated as dynamic", -1, 9, false, false},
		{"mismatch: too few classes", 8, 9, false, true},
		{"mismatch: too many classes", 20, 9, false, true},
		{"mismatch: off by one below M+1", 9, 9, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTrailing, err := validateClassCount(tc.outClasses, tc.numKeys, model, keys)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateClassCount(%d, %d): want error, got nil", tc.outClasses, tc.numKeys)
				}
				if !strings.Contains(err.Error(), "localocr:") {
					t.Errorf("error should carry localocr: prefix; got: %v", err)
				}
				if !strings.Contains(err.Error(), model) || !strings.Contains(err.Error(), keys) {
					t.Errorf("error should name both model %q and keys %q paths; got: %v", model, keys, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateClassCount(%d, %d): unexpected error: %v", tc.outClasses, tc.numKeys, err)
			}
			if gotTrailing != tc.wantTrailingSpace {
				t.Errorf("trailingSpace: got %v want %v", gotTrailing, tc.wantTrailingSpace)
			}
		})
	}
}

// TestValidateClassCount_ErrorMessageShape locks the exact wording the review
// finding asked for, so a future refactor cannot silently drop the "want M+1
// or M+2" guidance operators rely on when diagnosing a bad asset pairing.
func TestValidateClassCount_ErrorMessageShape(t *testing.T) {
	_, err := validateClassCount(50, 9, "m.onnx", "k.txt")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	want := `localocr: model "m.onnx" outputs 50 classes but keys file "k.txt" has 9 entries (want 10 or 11)`
	if err.Error() != want {
		t.Errorf("error message:\ngot:  %s\nwant: %s", err.Error(), want)
	}
}

// TestValidateClassCount_PPOCRv5Arithmetic pins the exact numbers the shipped
// asset pair produces, so a future keys-file or model swap that silently
// changes the charset trips here instead of at a user's first scan.
// ppocrv5_dict.txt has 18,383 entries; the PP-OCRv5 server rec export emits
// 18,385 classes (18383 + blank + trailing space), which is the trailingSpace
// path. 18,384 (blank only) stays acceptable for a re-export without the space
// class; anything else is a hard error naming both counts.
func TestValidateClassCount_PPOCRv5Arithmetic(t *testing.T) {
	const (
		v5Keys  = 18383 // wc -l ppocrv5_dict.txt
		model   = "data/ocr/PP-OCRv5_server_rec_infer.onnx"
		keyfile = "data/ocr/ppocrv5_dict.txt"
	)

	t.Run("18385 classes accepted with trailing space", func(t *testing.T) {
		trailingSpace, err := validateClassCount(v5Keys+2, v5Keys, model, keyfile)
		if err != nil {
			t.Fatalf("validateClassCount(%d, %d): unexpected error: %v", v5Keys+2, v5Keys, err)
		}
		if !trailingSpace {
			t.Errorf("trailingSpace: got false, want true for %d classes over %d keys", v5Keys+2, v5Keys)
		}
	})

	t.Run("18384 classes accepted without trailing space", func(t *testing.T) {
		trailingSpace, err := validateClassCount(v5Keys+1, v5Keys, model, keyfile)
		if err != nil {
			t.Fatalf("validateClassCount(%d, %d): unexpected error: %v", v5Keys+1, v5Keys, err)
		}
		if trailingSpace {
			t.Errorf("trailingSpace: got true, want false for %d classes over %d keys", v5Keys+1, v5Keys)
		}
	})

	for _, bad := range []int{v5Keys + 3, v5Keys - 1} {
		t.Run(fmt.Sprintf("%d classes rejected", bad), func(t *testing.T) {
			_, err := validateClassCount(bad, v5Keys, model, keyfile)
			if err == nil {
				t.Fatalf("validateClassCount(%d, %d): want error, got nil", bad, v5Keys)
			}
			// The message must name BOTH numbers so an operator can tell at a
			// glance which of the two assets is the odd one out.
			msg := err.Error()
			for _, want := range []string{strconv.Itoa(bad), strconv.Itoa(v5Keys), model, keyfile} {
				if !strings.Contains(msg, want) {
					t.Errorf("error should mention %q; got: %v", want, err)
				}
			}
		})
	}
}

// --- Finding 2: Close/recognize data race (docs/DECISIONS.md D26) --------
//
// Close destroys and nils e.session; recognize runs e.session.Run. Both must
// serialize on e.mu, or a Close on the shutdown-timeout path can free the native
// ORT session while a recognize is mid-Run — a data race that can segfault in the
// C layer. These tests exercise the closed-engine path (nil session) so they need
// no real ONNX Runtime: they lock in the mutex invariant, the clean closed error,
// and Close idempotency. The concurrent case is the one `go test -race` watches.

// oneLineCrop builds a minimal valid single-line IDCrop (a small black-on-white
// JPEG) so ReadLines gets past its IsZero/decode guards and into recognize.
func oneLineCrop(t *testing.T) imaging.IDCrop {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 64, 24))
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			img.Set(x, y, color.White)
		}
	}
	// A dark bar in the middle rows so splitLines finds one band.
	for y := 8; y < 16; y++ {
		for x := 8; x < 56; x++ {
			img.Set(x, y, color.Gray{Y: 0})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode crop: %v", err)
	}
	crop, err := imaging.LoadIDCrop("run/idcrop/x.jpg", buf.Bytes())
	if err != nil {
		t.Fatalf("LoadIDCrop: %v", err)
	}
	return crop
}

// TestClose_IdempotentOnNilSession proves Close is safe to call more than once
// (the deferred Close in main.go plus any explicit Close): the second call sees a
// nil session under the lock and is a clean no-op.
func TestClose_IdempotentOnNilSession(t *testing.T) {
	e := &Engine{} // session == nil, as after a prior Close
	if err := e.Close(); err != nil {
		t.Fatalf("first Close on nil session: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close (idempotency): %v", err)
	}
	// Close on a nil *Engine must also be safe (defer-friendly).
	var ep *Engine
	if err := ep.Close(); err != nil {
		t.Fatalf("Close on nil *Engine: %v", err)
	}
}

// TestReadLines_ClosedEngineReturnsCleanError proves recognize surfaces the
// sentinel errEngineClosed (via ReadLines) instead of dereferencing a nil
// session. This is the exact behavior the shutdown-timeout path relies on.
func TestReadLines_ClosedEngineReturnsCleanError(t *testing.T) {
	e := &Engine{} // closed / never-opened: session == nil
	_, err := e.ReadLines(context.Background(), oneLineCrop(t))
	if err == nil {
		t.Fatal("want an error from a closed engine, got nil")
	}
	if !errors.Is(err, errEngineClosed) {
		t.Fatalf("want errEngineClosed, got: %v", err)
	}
	if !strings.Contains(err.Error(), "localocr: engine closed") {
		t.Errorf("closed error wording changed: %v", err)
	}
}

// TestConcurrentReadLinesAndClose is the -race guard: many ReadLines racing a
// Close on the same Engine must not trigger the race detector and must not panic.
// With session == nil the recognize path short-circuits to errEngineClosed, so
// what's under test is purely the e.mu discipline around e.session shared between
// Close and recognize.
func TestConcurrentReadLinesAndClose(t *testing.T) {
	crop := oneLineCrop(t)
	for iter := 0; iter < 50; iter++ {
		e := &Engine{}
		var wg sync.WaitGroup
		for r := 0; r < 8; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Errors are expected (closed engine); we only care that this
				// never races Close or panics on a freed session.
				_, _ = e.ReadLines(context.Background(), crop)
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.Close()
		}()
		wg.Wait()
	}
}
