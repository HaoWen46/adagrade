package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Permissions are deliberately tight: blobs are student submissions (PII, D14).
const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// LocalDisk is a Store backed by a directory tree on the local filesystem.
// Writes are atomic (temp file + rename in the destination directory), so
// readers never observe partial blobs and concurrent Puts to one key leave
// exactly one intact payload. It is the v0 implementation behind the
// BlobStore seam (spec §2, D10/D15).
type LocalDisk struct {
	root string
}

var _ Store = (*LocalDisk)(nil)

// NewLocalDisk returns a LocalDisk rooted at root, creating the directory
// (mode 0700) if it does not exist.
func NewLocalDisk(root string) (*LocalDisk, error) {
	if root == "" {
		return nil, errors.New("blobstore: empty root")
	}
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return nil, fmt.Errorf("blobstore: create root: %w", err)
	}
	return &LocalDisk{root: root}, nil
}

// validateKey enforces the key contract: non-empty, relative, forward-slash
// separated, and already clean. Keys must be pre-cleaned by the caller —
// "a/./b" is rejected rather than normalized, so a key always names exactly
// one path and cannot escape the root.
func validateKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, `\`) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	if path.Clean(key) != key {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: %q", ErrInvalidKey, key)
		}
	}
	return nil
}

// blobPath maps a validated key to its on-disk location under root.
func (s *LocalDisk) blobPath(key string) string {
	return filepath.Join(s.root, filepath.FromSlash(key))
}

// Put streams r to the key, returning the SHA-256 (hex) and byte count of the
// stored content. The blob is written to a temp file in the destination
// directory, fsynced, then renamed over the final path, so an existing blob is
// replaced atomically and a failed Put leaves nothing behind.
func (s *LocalDisk) Put(ctx context.Context, key string, r io.Reader) (sha256hex string, size int64, err error) {
	if err := validateKey(key); err != nil {
		return "", 0, err
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	dst := s.blobPath(key)
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", 0, fmt.Errorf("blobstore: put %q: %w", key, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", 0, fmt.Errorf("blobstore: put %q: %w", key, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()        // no-op if already closed
			os.Remove(tmpName) // best-effort cleanup; never leave temp files
		}
	}()

	// os.CreateTemp creates mode 0600; make it explicit against umask drift.
	if err = tmp.Chmod(filePerm); err != nil {
		return "", 0, fmt.Errorf("blobstore: put %q: %w", key, err)
	}
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return "", 0, fmt.Errorf("blobstore: put %q: %w", key, err)
	}
	if err = tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("blobstore: put %q: %w", key, err)
	}
	if err = tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("blobstore: put %q: %w", key, err)
	}
	if err = os.Rename(tmpName, dst); err != nil {
		return "", 0, fmt.Errorf("blobstore: put %q: %w", key, err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// Get opens the blob at key for reading. A missing blob yields ErrNotFound.
func (s *LocalDisk) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(s.blobPath(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
		}
		return nil, fmt.Errorf("blobstore: get %q: %w", key, err)
	}
	return f, nil
}

var _ RandomAccess = (*LocalDisk)(nil)

// OpenRange implements RandomAccess: it opens the blob's backing file for
// random-access reads (*os.File is already an io.ReaderAt + io.Closer) and returns
// its size, so a consumer like zip.NewReader can read it in place without buffering
// the whole payload into heap (F4/F15). Key validation is enforced exactly as Get.
func (s *LocalDisk) OpenRange(ctx context.Context, key string) (ReaderAtCloser, int64, error) {
	if err := validateKey(key); err != nil {
		return nil, 0, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(s.blobPath(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, fmt.Errorf("%w: %q", ErrNotFound, key)
		}
		return nil, 0, fmt.Errorf("blobstore: openrange %q: %w", key, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("blobstore: openrange %q: %w", key, err)
	}
	return f, info.Size(), nil
}

// Delete removes the blob at key. Deleting a missing blob is a no-op (nil).
// Now-empty parent directories are pruned up to (but never including) the
// root, best-effort.
func (s *LocalDisk) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dst := s.blobPath(key)
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blobstore: delete %q: %w", key, err)
	}
	// Best-effort prune: os.Remove refuses non-empty directories, so stop at
	// the first parent that fails (still in use) or at the root.
	for dir := filepath.Dir(dst); dir != s.root; {
		if os.Remove(dir) != nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil
}

// Exists reports whether a blob is stored at key.
func (s *LocalDisk) Exists(ctx context.Context, key string) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := os.Stat(s.blobPath(key)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("blobstore: exists %q: %w", key, err)
	}
	return true, nil
}
