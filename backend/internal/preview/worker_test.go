package preview

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
)

func TestThumbWorkerUpdatesThumbnailWithoutChangingPreviewStatus(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-worker-video")

	gen := &fakeThumbGenerator{}
	drv := &previewFakeDrive{}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "/p/thumb/"+video.ID {
		t.Fatalf("thumbnail = %q, want generated thumb URL", got.ThumbnailURL)
	}
	if got.PreviewStatus != "pending" {
		t.Fatalf("preview status = %q, want pending", got.PreviewStatus)
	}
	if got.DurationSeconds != 42 {
		t.Fatalf("duration = %d, want probed duration", got.DurationSeconds)
	}
	if gen.thumbnailVideoID != video.ID {
		t.Fatalf("thumbnail video id = %q, want %q", gen.thumbnailVideoID, video.ID)
	}
	if gen.thumbnailDuration != 42 {
		t.Fatalf("thumbnail duration = %.1f, want 42", gen.thumbnailDuration)
	}
	if drv.streamFileID != video.FileID {
		t.Fatalf("stream file id = %q, want %q", drv.streamFileID, video.FileID)
	}
}

func TestThumbWorkerFallsBackToLocalPreviewWhenDriveStreamFails(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "thumb-worker-local-preview")
	localPreview := filepath.Join(t.TempDir(), "preview.mp4")
	if err := os.WriteFile(localPreview, []byte("preview"), 0o644); err != nil {
		t.Fatalf("write local preview: %v", err)
	}
	video.PreviewLocal = localPreview
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("update video: %v", err)
	}

	gen := &fakeThumbGenerator{}
	drv := &previewFakeDrive{streamErr: errors.New("remote unavailable")}
	worker := NewThumbWorker(gen, cat, drv)

	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "/p/thumb/"+video.ID {
		t.Fatalf("thumbnail = %q, want generated thumb URL", got.ThumbnailURL)
	}
	if gen.thumbnailURL != localPreview {
		t.Fatalf("thumbnail source = %q, want local preview %q", gen.thumbnailURL, localPreview)
	}
}

func TestPreviewWorkerGeneratesTeaserWithoutReplacingExistingThumbnail(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-worker-video")
	video.ThumbnailURL = "https://thumbnail.example/original.jpg"
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("update video: %v", err)
	}

	gen := &fakeTeaserGenerator{}
	drv := &previewFakeDrive{}
	worker := NewWorker(gen, cat, drv, "")

	worker.process(ctx, video)

	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.ThumbnailURL != "https://thumbnail.example/original.jpg" {
		t.Fatalf("thumbnail = %q, want existing thumbnail unchanged", got.ThumbnailURL)
	}
	if got.PreviewStatus != "ready" {
		t.Fatalf("preview status = %q, want ready", got.PreviewStatus)
	}
	if got.PreviewLocal != "/tmp/"+video.ID+".mp4" {
		t.Fatalf("preview local = %q, want moved teaser path", got.PreviewLocal)
	}
}

func TestPreviewWorkerRemovesPreviousLocalTeaserAfterNewTeaserIsReady(t *testing.T) {
	ctx := context.Background()
	cat, video := seedPreviewTestVideo(t, "preview-cleanup-video")
	oldPath := filepath.Join(t.TempDir(), "old-teaser.mp4")
	if err := os.WriteFile(oldPath, []byte("old teaser"), 0o644); err != nil {
		t.Fatalf("write old teaser: %v", err)
	}
	video.PreviewLocal = oldPath
	video.PreviewStatus = "ready"
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("update video: %v", err)
	}

	gen := &fakeTeaserGenerator{
		localPath: filepath.Join(t.TempDir(), "new-teaser.mp4"),
	}
	drv := &previewFakeDrive{}
	worker := NewWorker(gen, cat, drv, "")

	worker.process(ctx, video)

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old teaser still exists or stat failed with unexpected error: %v", err)
	}
	got, err := cat.GetVideo(ctx, video.ID)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if got.PreviewLocal != gen.localPath {
		t.Fatalf("preview local = %q, want %q", got.PreviewLocal, gen.localPath)
	}
}

func TestPreviewWorkerRateLimitLeavesCurrentPendingAndSkipsNextVideo(t *testing.T) {
	ctx := context.Background()
	cat, first := seedPreviewTestVideo(t, "preview-rate-limit-1")
	second := *first
	second.ID = "preview-rate-limit-2"
	second.FileID = "file-id-2"
	if err := cat.UpsertVideo(ctx, &second); err != nil {
		t.Fatalf("seed second video: %v", err)
	}

	gen := &fakeTeaserGenerator{
		generateErr: &drives.RateLimitError{
			Provider:   "onedrive",
			RetryAfter: 2 * time.Hour,
			Err:        errors.New("429 Too Many Requests"),
		},
	}
	drv := &previewFakeDrive{}
	worker := NewWorker(gen, cat, drv, "")

	worker.process(ctx, first)
	gotFirst, err := cat.GetVideo(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first video: %v", err)
	}
	if gotFirst.PreviewStatus != "pending" {
		t.Fatalf("first preview status = %q, want pending after rate limit", gotFirst.PreviewStatus)
	}
	if gen.generateCalls != 1 {
		t.Fatalf("generate calls = %d, want 1", gen.generateCalls)
	}

	gen.generateErr = nil
	worker.process(ctx, &second)
	gotSecond, err := cat.GetVideo(ctx, second.ID)
	if err != nil {
		t.Fatalf("get second video: %v", err)
	}
	if gotSecond.PreviewStatus != "pending" {
		t.Fatalf("second preview status = %q, want pending while drive is cooling down", gotSecond.PreviewStatus)
	}
	if gen.generateCalls != 1 {
		t.Fatalf("generate calls = %d, want second video skipped during cooldown", gen.generateCalls)
	}
}

func seedPreviewTestVideo(t *testing.T, id string) (*catalog.Catalog, *catalog.Video) {
	t.Helper()
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	video := &catalog.Video{
		ID:            id,
		DriveID:       "drive-id",
		FileID:        "file-id",
		Title:         "Clip",
		PreviewStatus: "pending",
		PublishedAt:   time.Now(),
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := cat.UpsertVideo(ctx, video); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	return cat, video
}

type fakeThumbGenerator struct {
	thumbnailVideoID  string
	thumbnailDuration float64
	thumbnailURL      string
}

func (g *fakeThumbGenerator) Probe(context.Context, *drives.StreamLink) (float64, error) {
	return 42, nil
}

func (g *fakeThumbGenerator) GenerateThumbnail(_ context.Context, link *drives.StreamLink, videoID string, duration float64) (string, error) {
	g.thumbnailVideoID = videoID
	g.thumbnailDuration = duration
	if link != nil {
		g.thumbnailURL = link.URL
	}
	return "/tmp/" + videoID + ".jpg", nil
}

type fakeTeaserGenerator struct {
	localPath     string
	generateErr   error
	generateCalls int
}

func (g *fakeTeaserGenerator) Probe(context.Context, *drives.StreamLink) (float64, error) {
	return 0, nil
}

func (g *fakeTeaserGenerator) Generate(context.Context, *drives.StreamLink, float64) (string, error) {
	g.generateCalls++
	if g.generateErr != nil {
		return "", g.generateErr
	}
	return "/tmp/source-teaser.mp4", nil
}

func (g *fakeTeaserGenerator) MoveToLocal(_ string, videoID string) (string, error) {
	if g.localPath != "" {
		return g.localPath, nil
	}
	return "/tmp/" + videoID + ".mp4", nil
}

type previewFakeDrive struct {
	streamFileID string
	streamErr    error
}

func (d *previewFakeDrive) Kind() string { return "fake" }
func (d *previewFakeDrive) ID() string   { return "drive-id" }
func (d *previewFakeDrive) Init(context.Context) error {
	return nil
}
func (d *previewFakeDrive) List(context.Context, string) ([]drives.Entry, error) {
	return nil, nil
}
func (d *previewFakeDrive) Stat(context.Context, string) (*drives.Entry, error) {
	return nil, drives.ErrNotSupported
}
func (d *previewFakeDrive) StreamURL(_ context.Context, fileID string) (*drives.StreamLink, error) {
	d.streamFileID = fileID
	if d.streamErr != nil {
		return nil, d.streamErr
	}
	return &drives.StreamLink{URL: "https://video.example/clip.mp4"}, nil
}
func (d *previewFakeDrive) Upload(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *previewFakeDrive) EnsureDir(context.Context, string) (string, error) {
	return "", drives.ErrNotSupported
}
func (d *previewFakeDrive) RootID() string { return "root" }
