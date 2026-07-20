package scan

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

type fx struct {
	svc *Service
	st  *store.Store
	aid int64
	ctx context.Context
	// recorded enqueues
	splits     *[]int64
	renders    *[]renderChunk
	identifies *[]int64
	promotes   *[]PromotePage
}

type renderChunk struct {
	SourceID int64
	PageIDs  []int64
}

func setup(t *testing.T) fx {
	t.Helper()
	st := storetest.Fresh(t)
	blobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ing := &ingest.Service{Store: st, Blobs: blobs, Renderer: render.NewFake(1)}
	svc := &Service{Store: st, Blobs: blobs, Renderer: render.NewFake(3), Ingest: ing}
	f := fx{svc: svc, st: st, ctx: context.Background(),
		splits: &[]int64{}, renders: &[]renderChunk{}, identifies: &[]int64{}, promotes: &[]PromotePage{}}
	svc.EnqueueSplit = func(_ context.Context, _ pgx.Tx, ids []int64) error {
		*f.splits = append(*f.splits, ids...)
		return nil
	}
	svc.EnqueueRenderPages = func(_ context.Context, _ pgx.Tx, sourceID int64, pageIDs []int64) error {
		*f.renders = append(*f.renders, renderChunk{SourceID: sourceID, PageIDs: pageIDs})
		return nil
	}
	svc.EnqueueIdentifyPages = func(_ context.Context, _ pgx.Tx, ids []int64) error {
		*f.identifies = append(*f.identifies, ids...)
		return nil
	}
	svc.EnqueuePromotePages = func(_ context.Context, _ pgx.Tx, items []PromotePage) error {
		*f.promotes = append(*f.promotes, items...)
		return nil
	}

	// actor=1 (created_by FK on scan_batches): the tests below all pass the
	// literal actor 1, so seed the first user in this fresh schema — IDENTITY
	// assigns it id 1.
	if _, err := st.Q.CreateUser(f.ctx, db.CreateUserParams{
		Email: "grader@x.edu", DisplayName: "Grader", Role: "ta", Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	// assessment + 3 problems + roster (synthetic 9-char NTU-format ids so the
	// local-OCR PickID length-5 floor accepts them; names are synthetic).
	a, err := st.Q.CreateAssessment(f.ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Scan Exam"})
	if err != nil {
		t.Fatal(err)
	}
	f.aid = a.ID
	for n := 1; n <= 3; n++ {
		mp, err := store.Num("10")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Q.CreateProblem(f.ctx, db.CreateProblemParams{
			AssessmentID: f.aid, Number: int32(n), Title: "Q", MaxPoints: mp, Position: int32(n),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []struct{ ext, name string }{
		{"B11902001", "王小明"}, {"B11902002", "李大華"},
	} {
		if _, err := st.Q.UpsertStudent(f.ctx, db.UpsertStudentParams{
			StudentID: s.ext, Name: s.name, Email: s.ext + "@x.edu",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func addRegions(f fx, t *testing.T) {
	t.Helper()
	for _, k := range []string{"student_id", "name", "problem_id"} {
		if _, err := f.st.Q.CreateIDRegion(f.ctx, db.CreateIDRegionParams{
			AssessmentID: f.aid, Kind: k,
			X: 0.05, Y: 0.02, W: 0.25, H: 0.06, Color: "#4a4a4a", Padding: 0.01,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.Set(0, 0, color.Black)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCreateBatch_RequiresAllThreeRegions(t *testing.T) {
	f := setup(t)
	// only two kinds drawn
	for _, k := range []string{"student_id", "name"} {
		if _, err := f.st.Q.CreateIDRegion(f.ctx, db.CreateIDRegionParams{
			AssessmentID: f.aid, Kind: k, X: 0.05, Y: 0.02, W: 0.25, H: 0.06,
			Color: "#4a4a4a", Padding: 0.01,
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{}, []SourceUpload{
		{Filename: "run1.pdf", R: strings.NewReader("%PDF-1")},
	}, nil, 1)
	if err != ErrRegionsIncomplete {
		t.Fatalf("want ErrRegionsIncomplete, got %v", err)
	}
}

func TestCreateBatch_OCREnabledRequiresProvider(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	_, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{OCREnabled: true}, []SourceUpload{
		{Filename: "run1.pdf", R: strings.NewReader("%PDF-1")},
	}, nil, 1)
	if !errors.Is(err, ErrOCRProviderRequired) {
		t.Fatalf("want ErrOCRProviderRequired, got %v", err)
	}
	batches, lerr := f.st.Q.ListScanBatches(f.ctx, f.aid)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(batches) != 0 {
		t.Fatalf("no batch row must be created, got %d", len(batches))
	}
	// OCR off needs no provider (the D24 ladder ends at the local rung / human).
	if _, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{}, []SourceUpload{
		{Filename: "run2.pdf", R: strings.NewReader("%PDF-2")},
	}, nil, 1); err != nil {
		t.Fatalf("OCR-disabled batch must not need a provider: %v", err)
	}
}

func TestCreateBatch_LoosePDFs_CreatesSourcesAndEnqueuesSplit(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	view, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{OCREnabled: true, OCRProvider: "p", OCRModel: "m"},
		[]SourceUpload{
			{Filename: "run1.pdf", R: strings.NewReader("%PDF-1 run one")},
			{Filename: "run2.pdf", R: strings.NewReader("%PDF-1 run two")},
			{Filename: "notes.txt", R: strings.NewReader("nope")},
		}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if view.Created != 2 {
		t.Fatalf("created = %d, want 2", view.Created)
	}
	if len(view.Skipped) != 1 || view.Skipped[0].Reason != "unknown_extension" {
		t.Fatalf("skipped = %+v", view.Skipped)
	}
	if len(*f.splits) != 2 {
		t.Fatalf("split enqueues = %d, want 2", len(*f.splits))
	}
	srcs, err := f.st.Q.ListScanSourcesForBatch(f.ctx, view.Batch.ID)
	if err != nil || len(srcs) != 2 {
		t.Fatalf("sources = %d (%v), want 2", len(srcs), err)
	}
	if srcs[0].SourceKind != "pdf" {
		t.Fatalf("kind = %s", srcs[0].SourceKind)
	}
}

func TestCreateBatch_DuplicateSourceSkipped(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	same := "%PDF-1 identical bytes"
	view, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{}, []SourceUpload{
		{Filename: "a.pdf", R: strings.NewReader(same)},
		{Filename: "b.pdf", R: strings.NewReader(same)},
	}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if view.Created != 1 || len(view.Skipped) != 1 || view.Skipped[0].Reason != "duplicate" {
		t.Fatalf("view = %+v", view)
	}
}

func TestCreateBatch_SourceOverCap(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	old := MaxSourceBytes
	MaxSourceBytes = 16
	t.Cleanup(func() { MaxSourceBytes = old })
	view, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{}, []SourceUpload{
		{Filename: "huge.pdf", R: strings.NewReader("%PDF-1 way past the sixteen byte cap")},
	}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if view.Created != 0 || len(view.Skipped) != 1 || view.Skipped[0].Reason != "too_large" {
		t.Fatalf("view = %+v", view)
	}
}

func TestExpand_ZipIntoSources(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	z := zipOf(t, map[string][]byte{
		"page-001.png":      pngBytes(t, 40, 60),
		"run.pdf":           []byte("%PDF-1 zipped run"),
		"__MACOSX/junk.png": {1, 2, 3},
		"cover.txt":         []byte("skip me"),
	})
	view, err := f.svc.CreateBatch(f.ctx, f.aid, NewBatch{}, nil, bytes.NewReader(z), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Expand(f.ctx, view.Batch.ID); err != nil {
		t.Fatal(err)
	}
	srcs, err := f.st.Q.ListScanSourcesForBatch(f.ctx, view.Batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 {
		t.Fatalf("sources = %d, want 2 (png + pdf; macosx + txt skipped)", len(srcs))
	}
	if len(*f.splits) != 2 {
		t.Fatalf("split enqueues = %d, want 2", len(*f.splits))
	}
}
