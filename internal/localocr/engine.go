package localocr

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/ocr"
)

// Recognizer input geometry (docs/DECISIONS.md D24). The ch_PP-OCRv4 mobile rec
// model takes NCHW [1,3,H,W]; H is fixed at 48 and W varies per line (dynamic
// axis), which is why the Engine uses a DynamicAdvancedSession.
const (
	recHeight = 48  // model input height
	recMaxW   = 640 // cap on per-line width
	recMinW   = 16  // pad narrower lines up to this floor
)

// Config points the Engine at its three on-disk dependencies. All are required;
// none are embedded so the ~11MB model and the shared library stay out of the
// binary and the repo (assets are provisioned separately).
type Config struct {
	ModelPath          string // ch_PP-OCRv4_rec_infer.onnx
	KeysPath           string // ppocr_keys_v1.txt (one glyph per line)
	ONNXRuntimeLibPath string // libonnxruntime.{so,dylib,dll}
}

// Engine is an offline ocr.Reader backed by onnxruntime. It is safe for
// concurrent use: identify workers may call ReadLines simultaneously, and a
// mutex serializes the underlying (millisecond-scale) inference, which the ORT
// session does not itself guarantee to be reentrant.
type Engine struct {
	keys          []rune
	trailingSpace bool // model output width == len(keys)+2 (extra ' ' class)
	inputName     string
	outputName    string

	mu      sync.Mutex // guards session.Run
	session *ort.DynamicAdvancedSession
}

var _ ocr.Reader = (*Engine)(nil)

// errEngineClosed is returned by recognize when the session has been Destroyed by
// a concurrent (or prior) Close. Callers on the shutdown path get this clean error
// instead of a nil-session dereference / native segfault (D26).
var errEngineClosed = errors.New("localocr: engine closed")

// ortInit guards the process-global onnxruntime environment. onnxruntime_go
// dlopens one shared library per process and InitializeEnvironment must be
// called exactly once; a sync.Once lets a second Engine reuse the environment.
var (
	ortInit     sync.Once
	ortInitErr  error
	ortInitPath string // the lib path the environment was initialized with
)

// initORT initializes the shared onnxruntime environment on first use, binding
// it to libPath. A later call with a DIFFERENT path is an error: the library is
// already dlopen'd and cannot be swapped, so surfacing it beats silently using
// the first path.
func initORT(libPath string) error {
	ortInit.Do(func() {
		ort.SetSharedLibraryPath(libPath)
		if err := ort.InitializeEnvironment(); err != nil {
			ortInitErr = fmt.Errorf("localocr: initialize onnxruntime (lib %q): %w", libPath, err)
			return
		}
		ortInitPath = libPath
	})
	if ortInitErr != nil {
		return ortInitErr
	}
	if ortInitPath != libPath {
		return fmt.Errorf("localocr: onnxruntime already initialized with shared library %q; cannot re-initialize with %q (one library per process)", ortInitPath, libPath)
	}
	return nil
}

// New validates the config, loads the dictionary, initializes the onnxruntime
// environment, and opens a dynamic session. Missing files produce an error that
// names the offending path. The caller owns the returned Engine and must Close
// it.
func New(cfg Config) (*Engine, error) {
	if cfg.ModelPath == "" || cfg.KeysPath == "" || cfg.ONNXRuntimeLibPath == "" {
		return nil, fmt.Errorf("localocr: Config requires ModelPath, KeysPath and ONNXRuntimeLibPath (got model=%q keys=%q lib=%q)",
			cfg.ModelPath, cfg.KeysPath, cfg.ONNXRuntimeLibPath)
	}
	if err := mustExist(cfg.ModelPath, "model"); err != nil {
		return nil, err
	}
	keys, err := loadKeys(cfg.KeysPath)
	if err != nil {
		return nil, err
	}
	if err := mustExist(cfg.ONNXRuntimeLibPath, "onnxruntime shared library"); err != nil {
		return nil, err
	}

	if err := initORT(cfg.ONNXRuntimeLibPath); err != nil {
		return nil, err
	}

	// The PP-OCRv4 rec export names its input "x" and its (softmaxed) output
	// "softmax_11.tmp_0"; discover them from metadata so we do not hard-code a
	// name that a re-export might change.
	inName, outName, outClasses, err := modelIO(cfg.ModelPath)
	if err != nil {
		return nil, err
	}
	// When the model's output class count is statically known, it MUST be
	// len(keys)+1 (blank) or len(keys)+2 (blank + trailing space class) — any
	// other count means the model and the keys file were not exported
	// together (e.g. a keys file swapped in from a different model release).
	// Silently proceeding would leave classToRune clamping every real
	// out-of-range class to '?' or blank, producing plausible-looking but
	// wrong text instead of a clear startup error, so this is checked before
	// opening a session at all.
	trailingSpace, err := validateClassCount(outClasses, len(keys), cfg.ModelPath, cfg.KeysPath)
	if err != nil {
		return nil, err
	}

	sess, err := ort.NewDynamicAdvancedSession(cfg.ModelPath,
		[]string{inName}, []string{outName}, nil)
	if err != nil {
		return nil, fmt.Errorf("localocr: open session for %q: %w", cfg.ModelPath, err)
	}

	eng := &Engine{
		keys:          keys,
		inputName:     inName,
		outputName:    outName,
		session:       sess,
		trailingSpace: trailingSpace,
	}
	return eng, nil
}

// validateClassCount checks the model's statically-known output class count
// against the keys file's entry count, returning the resolved trailingSpace
// flag. PP-OCR CTC width is len(keys)+1 (blank) normally, or len(keys)+2 when
// an export appends a trailing space class. outClasses<=0 means the count is
// only known dynamically (a fully dynamic output dim) — there is nothing to
// check yet, so trailingSpace defaults to false (mirroring the prior
// behavior) and classToRune clamps at decode time as before. Any statically
// known count that is neither M+1 nor M+2 is a hard config error naming both
// paths (no PII: these are on-disk asset paths, not student data).
func validateClassCount(outClasses, numKeys int, modelPath, keysPath string) (trailingSpace bool, err error) {
	switch {
	case outClasses <= 0:
		return false, nil // dynamic dim — unknown until first Run; nothing to validate yet
	case outClasses == numKeys+1:
		return false, nil
	case outClasses == numKeys+2:
		return true, nil
	default:
		return false, fmt.Errorf(
			"localocr: model %q outputs %d classes but keys file %q has %d entries (want %d or %d)",
			modelPath, outClasses, keysPath, numKeys, numKeys+1, numKeys+2)
	}
}

// Close releases the session. The shared onnxruntime environment is process-
// global and intentionally left initialized for any other Engine.
//
// Concurrency invariant: Close takes e.mu around Destroy + the nil assignment so
// it cannot race a concurrent recognize (which holds e.mu across session.Run).
// Without the lock, Close on a shutdown-timeout path could Destroy/nil the native
// session while recognize was mid-Run, a data race that can segfault in the ORT
// C layer. recognize re-checks e.session != nil under the same lock and returns a
// clean "engine closed" error instead of dereferencing a nil session. Close is
// idempotent: a second call sees a nil session and is a no-op (verified against
// concurrent ReadLines + Close in the -race test). See docs/DECISIONS.md D26.
func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session == nil {
		return nil
	}
	err := e.session.Destroy()
	e.session = nil
	if err != nil {
		return fmt.Errorf("localocr: close session: %w", err)
	}
	return nil
}

// closed reports whether the session has been Destroyed, reading e.session under
// e.mu so it never races a concurrent Close. It is only a snapshot: a Close may
// land immediately after it returns false, which is why recognize re-checks under
// the lock before calling Run (D26).
func (e *Engine) closed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.session == nil
}

// ReadLines decodes the crop JPEG, splits it into line bands, and recognizes
// each band. When the single pass comes back weak (best line confidence below
// retryConfidence, or no line read at all) it retries on up to two alternative
// preprocessings of the same crop and keeps the highest-confidence result —
// see retry.go for the decision rule and the live evidence behind it. It never
// logs the crop or the recognized text (D14 PII). The returned lines are in
// top-to-bottom order; empty recognitions are skipped.
func (e *Engine) ReadLines(ctx context.Context, crop imaging.IDCrop) ([]ocr.Line, error) {
	if crop.IsZero() {
		return nil, fmt.Errorf("localocr: empty IDCrop")
	}
	src, err := jpeg.Decode(bytes.NewReader(crop.JPEG()))
	if err != nil {
		return nil, fmt.Errorf("localocr: decode crop JPEG: %w", err)
	}
	return readLinesRetry(ctx, toGray(src), e.recognize)
}

// ReadLattices is ReadLines with the model's per-timestep class distribution
// kept: same band split, same retry ladder, but each line also carries the
// top-K CTC lattice of the WINNING variant's inference pass, so a lexicon
// scorer can ask how well an arbitrary candidate string explains the same
// pixels the reported text came from. Low-confidence lines are returned like
// any other (with their lattices); the caller decides what to do with them.
func (e *Engine) ReadLattices(ctx context.Context, crop imaging.IDCrop) ([]LineLattice, error) {
	if crop.IsZero() {
		return nil, fmt.Errorf("localocr: empty IDCrop")
	}
	src, err := jpeg.Decode(bytes.NewReader(crop.JPEG()))
	if err != nil {
		return nil, fmt.Errorf("localocr: decode crop JPEG: %w", err)
	}
	return retryLadder(ctx, toGray(src), func(ctx context.Context, g *image.Gray) ([]LineLattice, error) {
		return readBandsWith(ctx, g, e.recognizeLattice)
	}, bestLatticeConfidence)
}

// recognize runs one line-band image through preprocess -> session -> CTC.
func (e *Engine) recognize(img image.Image) (string, float64, error) {
	rows, err := e.runLine(img)
	if err != nil {
		return "", 0, err
	}
	text, conf := e.decode(rows)
	return text, conf, nil
}

// recognizeLattice runs one line band and returns its greedy decode alongside a
// lattice built from the SAME rows, for the retry ladder's band loop (the bool
// reports whether anything was read, mirroring recognize's empty-text skip).
//
// The lattice is compressed here, per pass, rather than carrying raw rows
// through the ladder and compressing only the winner: a lattice is ~40KB where
// the rows behind it are ~2MB, and identify workers run concurrently, so the
// discarded work on the (rare) retry path buys a bounded memory footprint.
func (e *Engine) recognizeLattice(img image.Image) (LineLattice, bool, error) {
	rows, err := e.runLine(img)
	if err != nil {
		return LineLattice{}, false, err
	}
	text, conf := e.decode(rows)
	// NewLattice takes the RAW rows: it re-applies the softmaxed/logits
	// decision itself and needs the logits to compute log-softmax directly.
	return LineLattice{Lattice: NewLattice(rows, latticeTopK), Text: text, Confidence: conf}, text != "", nil
}

// decode greedy-decodes raw model rows, normalizing first when the export does
// not end in a Softmax node.
func (e *Engine) decode(rows [][]float32) (string, float64) {
	if len(rows) > 0 && !looksSoftmaxed(rows[0]) {
		rows = softmaxRows(rows)
	}
	return ctcGreedyDecode(rows, e.keys, e.trailingSpace)
}

// runLine runs one line-band image through preprocess -> session and returns
// the model's raw [T][C] output rows. It is the single inference path: both
// the text and the lattice callers go through it, so the closed-engine guard
// and the mutex discipline (D26) exist in exactly one place.
func (e *Engine) runLine(img image.Image) ([][]float32, error) {
	// Fast closed-check before allocating a native ORT tensor: a Close may have
	// already Destroyed the session, in which case there is nothing to run. The
	// authoritative re-check happens again under e.mu just before Run so a Close
	// racing this call cannot free the session mid-Run (D26).
	if e.closed() {
		return nil, errEngineClosed
	}
	data, shape := preprocess(img, recHeight, recMaxW, recMinW)
	in, err := ort.NewTensor(ort.NewShape(shape...), data)
	if err != nil {
		return nil, fmt.Errorf("localocr: build input tensor: %w", err)
	}
	defer in.Destroy()

	outputs := []ort.Value{nil} // auto-allocated by Run
	// Hold e.mu across the nil-check and Run so a concurrent Close (which takes
	// e.mu around Destroy + nil, D26) can never free the native session mid-Run.
	e.mu.Lock()
	if e.session == nil {
		e.mu.Unlock()
		return nil, errEngineClosed
	}
	err = e.session.Run([]ort.Value{in}, outputs)
	e.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("localocr: inference: %w", err)
	}
	out := outputs[0]
	defer out.Destroy()

	return outputToRows(out)
}

// outputToRows reshapes a [1, T, C] float32 output Value into a [T][C] matrix.
// The data is COPIED out of the tensor: the returned rows outlive the ort.Value
// (runLine Destroys it before returning), and slicing the native buffer would
// leave the caller reading freed memory.
func outputToRows(v ort.Value) ([][]float32, error) {
	t, ok := v.(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("localocr: unexpected output tensor type %T (want float32)", v)
	}
	shape := t.GetShape()
	if len(shape) != 3 || shape[0] != 1 {
		return nil, fmt.Errorf("localocr: unexpected output shape %v (want [1,T,C])", shape)
	}
	T := int(shape[1])
	C := int(shape[2])
	flat := t.GetData()
	if len(flat) < T*C {
		return nil, fmt.Errorf("localocr: output data too short: have %d want %d", len(flat), T*C)
	}
	buf := make([]float32, T*C)
	copy(buf, flat[:T*C])
	rows := make([][]float32, T)
	for i := 0; i < T; i++ {
		rows[i] = buf[i*C : (i+1)*C : (i+1)*C]
	}
	return rows, nil
}

// modelIO reads the model's single input/output names and, when statically
// known, the output's trailing class dimension. Falls back to the PP-OCRv4
// default names when metadata is unavailable.
func modelIO(path string) (inName, outName string, outClasses int, err error) {
	ins, outs, e := ort.GetInputOutputInfo(path)
	if e != nil {
		return "", "", 0, fmt.Errorf("localocr: read model io info for %q: %w", path, e)
	}
	if len(ins) == 0 || len(outs) == 0 {
		return "", "", 0, fmt.Errorf("localocr: model %q has %d inputs and %d outputs (want >=1 each)", path, len(ins), len(outs))
	}
	inName = ins[0].Name
	outName = outs[0].Name
	// Output dims: [batch, T, C]; C is the last dim if it is statically known
	// (>0). Dynamic dims are reported as <= 0.
	dims := outs[0].Dimensions
	if n := len(dims); n > 0 {
		if c := dims[n-1]; c > 0 {
			outClasses = int(c)
		}
	}
	return inName, outName, outClasses, nil
}

// mustExist returns a path-naming error if p is not a readable regular file.
func mustExist(p, what string) error {
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("localocr: %s not found at %q: %w", what, p, err)
	}
	if info.IsDir() {
		return fmt.Errorf("localocr: %s path %q is a directory, not a file", what, p)
	}
	return nil
}

// loadKeys reads a PP-OCR dictionary file: one glyph per line, no header. A
// trailing newline does not create a phantom empty entry. Each line is expected
// to be a single rune; multi-rune lines are stored by their first rune (the
// PP-OCR dict is one-glyph-per-line by construction).
func loadKeys(path string) ([]rune, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("localocr: open keys file %q: %w", path, err)
	}
	defer f.Close()

	var keys []rune
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// PP-OCR dict lines carry exactly one glyph; a space glyph is stored
		// verbatim. Skip only genuinely empty lines (e.g. a trailing newline).
		if line == "" {
			continue
		}
		rs := []rune(line)
		keys = append(keys, rs[0])
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("localocr: read keys file %q: %w", path, err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("localocr: keys file %q is empty", path)
	}
	return keys, nil
}

// toGray converts any image to *image.Gray, returning the input unchanged when
// it is already grayscale.
func toGray(src image.Image) *image.Gray {
	if g, ok := src.(*image.Gray); ok {
		return g
	}
	b := src.Bounds()
	g := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			g.Set(x, y, src.At(x, y))
		}
	}
	return g
}
