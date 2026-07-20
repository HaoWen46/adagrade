package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
)

func newTestStore(t *testing.T) (*LocalDisk, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "blobs")
	s, err := NewLocalDisk(root)
	if err != nil {
		t.Fatalf("NewLocalDisk(%q): %v", root, err)
	}
	return s, root
}

// findLeftovers returns any temp files (".tmp-*") left anywhere under root.
func findLeftovers(t *testing.T, root string) []string {
	t.Helper()
	var leftovers []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), ".tmp-") {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", root, err)
	}
	return leftovers
}

func TestNewLocalDiskCreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "b", "blobs")
	if _, err := NewLocalDisk(root); err != nil {
		t.Fatalf("NewLocalDisk: %v", err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("root %q is not a directory", root)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("root perm = %o, want 0700", got)
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		payload []byte
	}{
		{"simple", "hello.txt", []byte("hello, world")},
		{"nested key", "assessments/3/submissions/7.pdf", []byte("%PDF-1.7 fake")},
		{"empty payload", "empty.bin", nil},
		{"binary payload", "img/pg.png", []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0xff, 0x00}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, root := newTestStore(t)
			ctx := context.Background()

			gotSHA, gotSize, err := s.Put(ctx, tt.key, bytes.NewReader(tt.payload))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			wantSum := sha256.Sum256(tt.payload)
			if wantSHA := hex.EncodeToString(wantSum[:]); gotSHA != wantSHA {
				t.Errorf("Put sha = %s, want %s", gotSHA, wantSHA)
			}
			if gotSize != int64(len(tt.payload)) {
				t.Errorf("Put size = %d, want %d", gotSize, len(tt.payload))
			}

			rc, err := s.Get(ctx, tt.key)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, tt.payload) {
				t.Errorf("Get content = %q, want %q", got, tt.payload)
			}

			// PII lives here: files 0600, created dirs 0700.
			full := filepath.Join(root, filepath.FromSlash(tt.key))
			fi, err := os.Stat(full)
			if err != nil {
				t.Fatalf("stat blob: %v", err)
			}
			if perm := fi.Mode().Perm(); perm != 0o600 {
				t.Errorf("file perm = %o, want 0600", perm)
			}
			for dir := filepath.Dir(full); dir != filepath.Clean(root); dir = filepath.Dir(dir) {
				di, err := os.Stat(dir)
				if err != nil {
					t.Fatalf("stat dir %q: %v", dir, err)
				}
				if perm := di.Mode().Perm(); perm != 0o700 {
					t.Errorf("dir %q perm = %o, want 0700", dir, perm)
				}
			}

			if lo := findLeftovers(t, root); len(lo) > 0 {
				t.Errorf("temp files left behind: %v", lo)
			}
		})
	}
}

func TestPutOverwriteReplacesContent(t *testing.T) {
	s, root := newTestStore(t)
	ctx := context.Background()
	const key = "a/b/report.txt"

	if _, _, err := s.Put(ctx, key, strings.NewReader("version one")); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	v2 := []byte("version two — longer than the first payload")
	gotSHA, gotSize, err := s.Put(ctx, key, bytes.NewReader(v2))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	wantSum := sha256.Sum256(v2)
	if wantSHA := hex.EncodeToString(wantSum[:]); gotSHA != wantSHA {
		t.Errorf("overwrite sha = %s, want %s", gotSHA, wantSHA)
	}
	if gotSize != int64(len(v2)) {
		t.Errorf("overwrite size = %d, want %d", gotSize, len(v2))
	}

	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, v2) {
		t.Errorf("Get after overwrite = %q, want %q", got, v2)
	}
	if lo := findLeftovers(t, root); len(lo) > 0 {
		t.Errorf("temp files left behind: %v", lo)
	}
}

func TestPutFailedReaderLeavesNothing(t *testing.T) {
	s, root := newTestStore(t)
	ctx := context.Background()
	const key = "a/partial.bin"

	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader("some partial bytes"), iotest.ErrReader(boom))
	if _, _, err := s.Put(ctx, key, r); !errors.Is(err, boom) {
		t.Fatalf("Put with failing reader: err = %v, want wrapped %v", err, boom)
	}
	ok, err := s.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if ok {
		t.Errorf("key %q exists after failed Put", key)
	}
	if lo := findLeftovers(t, root); len(lo) > 0 {
		t.Errorf("temp files left behind after failed Put: %v", lo)
	}
}

func TestGetMissingIsErrNotFound(t *testing.T) {
	s, _ := newTestStore(t)
	_, err := s.Get(context.Background(), "no/such/key.pdf")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteIdempotentAndPrunesEmptyParents(t *testing.T) {
	s, root := newTestStore(t)
	ctx := context.Background()

	// Deleting a key that never existed is nil.
	if err := s.Delete(ctx, "never/was/here.txt"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}

	const key = "a/b/c/deep.txt"
	if _, _, err := s.Put(ctx, key, strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ok, err := s.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if ok {
		t.Errorf("key %q still exists after Delete", key)
	}
	// Now-empty parents a/b/c, a/b, a are pruned; root itself survives.
	for _, dir := range []string{"a/b/c", "a/b", "a"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir))); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("empty parent %q not pruned (stat err = %v)", dir, err)
		}
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("root removed by parent pruning: %v", err)
	}
	// Second delete of the same key: still nil.
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestDeleteKeepsNonEmptyParents(t *testing.T) {
	s, root := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.Put(ctx, "a/x.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put x: %v", err)
	}
	if _, _, err := s.Put(ctx, "a/y.txt", strings.NewReader("y")); err != nil {
		t.Fatalf("Put y: %v", err)
	}
	if err := s.Delete(ctx, "a/x.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); err != nil {
		t.Errorf("non-empty parent \"a\" was removed: %v", err)
	}
	rc, err := s.Get(ctx, "a/y.txt")
	if err != nil {
		t.Fatalf("sibling gone after Delete: %v", err)
	}
	rc.Close()
}

func TestExists(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	const key = "p/q.bin"

	ok, err := s.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists before Put: %v", err)
	}
	if ok {
		t.Errorf("Exists = true before Put")
	}
	if _, _, err := s.Put(ctx, key, strings.NewReader("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ok, err = s.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists after Put: %v", err)
	}
	if !ok {
		t.Errorf("Exists = false after Put")
	}
}

func TestInvalidKeys(t *testing.T) {
	invalid := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"absolute", "/abs"},
		{"traversal", "a/../../etc/passwd"},
		{"backslash", `a\b`},
		{"dotdot", ".."},
		{"dot segment (keys must be pre-cleaned)", "a/./b"},
		{"trailing slash", "a/b/"},
		{"empty segment", "a//b"},
	}
	s, root := newTestStore(t)
	ctx := context.Background()

	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := s.Put(ctx, tt.key, strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Put(%q) err = %v, want ErrInvalidKey", tt.key, err)
			}
			if _, err := s.Get(ctx, tt.key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Get(%q) err = %v, want ErrInvalidKey", tt.key, err)
			}
			if err := s.Delete(ctx, tt.key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Delete(%q) err = %v, want ErrInvalidKey", tt.key, err)
			}
			if _, err := s.Exists(ctx, tt.key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Exists(%q) err = %v, want ErrInvalidKey", tt.key, err)
			}
		})
	}

	// Nothing may have escaped or landed inside root.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("invalid keys created entries under root: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "etc")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("traversal key escaped root (stat err = %v)", err)
	}
}

func TestConcurrentPutsSameKeyNoCorruption(t *testing.T) {
	s, root := newTestStore(t)
	ctx := context.Background()
	const key = "hot/contended.bin"
	const n = 10

	// Distinct, self-identifying payloads large enough to catch interleaving.
	payloads := make([][]byte, n)
	shas := make(map[string]int, n)
	for i := range payloads {
		p := bytes.Repeat([]byte(fmt.Sprintf("payload-%02d|", i)), 8192)
		payloads[i] = p
		sum := sha256.Sum256(p)
		shas[hex.EncodeToString(sum[:])] = i
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sha, size, err := s.Put(ctx, key, bytes.NewReader(payloads[i]))
			if err != nil {
				errs[i] = err
				return
			}
			if _, ok := shas[sha]; !ok || size != int64(len(payloads[i])) {
				errs[i] = fmt.Errorf("goroutine %d: got sha=%s size=%d", i, sha, size)
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Put %d: %v", i, err)
		}
	}

	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	sum := sha256.Sum256(got)
	winner, ok := shas[hex.EncodeToString(sum[:])]
	if !ok {
		t.Fatalf("final content matches no payload (len=%d): corrupted by concurrent Puts", len(got))
	}
	if len(got) != len(payloads[winner]) {
		t.Fatalf("winner %d truncated: len=%d want %d", winner, len(got), len(payloads[winner]))
	}
	if lo := findLeftovers(t, root); len(lo) > 0 {
		t.Errorf("temp files left behind: %v", lo)
	}
}

// TestOpenRangeRoundTrip verifies the RandomAccess capability (F4/F15): OpenRange
// hands back a ReaderAtCloser positioned over the stored bytes, reports the correct
// size, reads at arbitrary offsets (what zip.NewReader needs), and returns
// ErrNotFound for a missing key with the same key validation as Get.
func TestOpenRangeRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	payload := []byte("the quick brown fox jumps over the lazy dog")
	if _, _, err := s.Put(ctx, "a/b/c.bin", bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	f, size, err := s.OpenRange(ctx, "a/b/c.bin")
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer f.Close()
	if size != int64(len(payload)) {
		t.Fatalf("size: got %d want %d", size, len(payload))
	}
	// Random-access read from the middle (offset 4, len 5 -> "quick").
	buf := make([]byte, 5)
	if _, err := f.ReadAt(buf, 4); err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != "quick" {
		t.Fatalf("ReadAt(4): got %q want %q", buf, "quick")
	}

	if _, _, err := s.OpenRange(ctx, "missing/key.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenRange(missing): got %v want ErrNotFound", err)
	}
	if _, _, err := s.OpenRange(ctx, "../escape"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("OpenRange(bad key): got %v want ErrInvalidKey", err)
	}
}
