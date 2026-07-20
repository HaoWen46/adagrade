// Package blobstore defines the content-storage seam (spec §2). Blobs hold
// student submission PDFs and page images (PII — see D10/D14/D15 in
// docs/DECISIONS.md): implementations must keep permissions tight and callers
// must never log blob contents, only keys and IDs.
package blobstore

import (
	"context"
	"errors"
	"io"
)

// Store is content storage behind a swappable seam (spec §2). Keys are app-defined
// slash-separated paths like "assessments/3/submissions/7.pdf".
type Store interface {
	Put(ctx context.Context, key string, r io.Reader) (sha256hex string, size int64, err error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
}

// RandomAccess is an OPTIONAL capability a Store may implement to hand out a
// seekable, random-access handle to a blob (F15/F4). It exists so a consumer that
// needs an io.ReaderAt — chiefly archive/zip's Reader, which requires it — can read
// a large blob in place instead of buffering the whole thing into heap. Callers
// type-assert for it and fall back to streaming Get → a temp file when it is
// absent, so no Store is obliged to implement it. The returned handle owns an open
// file/descriptor; the caller must Close it.
type RandomAccess interface {
	// OpenRange opens the blob at key for random-access reads, returning a handle
	// that is both io.ReaderAt (for zip.NewReader) and io.Closer, plus the blob's
	// total size. A missing blob yields ErrNotFound.
	OpenRange(ctx context.Context, key string) (f ReaderAtCloser, size int64, err error)
}

// ReaderAtCloser is the handle OpenRange returns: an io.ReaderAt (what zip.NewReader
// consumes) that must be Closed by the caller.
type ReaderAtCloser interface {
	io.ReaderAt
	io.Closer
}

var (
	// ErrNotFound is returned by Get when no blob exists at the given key.
	ErrNotFound = errors.New("blobstore: not found")

	// ErrInvalidKey is returned for keys that are empty, absolute, contain
	// backslashes, or are not pre-cleaned slash paths (no "." or ".."
	// segments, no empty segments, no trailing slash). Keys must be built
	// clean by the caller; the store never normalizes them.
	ErrInvalidKey = errors.New("blobstore: invalid key")
)
