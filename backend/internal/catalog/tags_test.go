package catalog

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestCreateTagAndClassifyAddsTagToMatchingExistingVideos(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯短发合集",
		Category:    "普通目录",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed matching video: %v", err)
	}
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-2",
		DriveID:     "drive",
		FileID:      "file-2",
		Title:       "普通标题",
		Category:    "普通目录",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed non-matching video: %v", err)
	}

	classified, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user")
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if classified != 1 {
		t.Fatalf("classified = %d, want 1", classified)
	}

	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get matching video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"清纯"}) {
		t.Fatalf("matching tags = %#v, want 清纯", got.Tags)
	}

	other, err := cat.GetVideo(ctx, "video-2")
	if err != nil {
		t.Fatalf("get non-matching video: %v", err)
	}
	if len(other.Tags) != 0 {
		t.Fatalf("non-matching tags = %#v, want none", other.Tags)
	}
}

func TestOpenMigratesLegacyVideosWithoutFileName(t *testing.T) {
	path := t.TempDir() + "/catalog.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`
CREATE TABLE videos (
	id               TEXT PRIMARY KEY,
	drive_id         TEXT NOT NULL,
	file_id          TEXT NOT NULL,
	content_hash     TEXT DEFAULT '',
	parent_id        TEXT,
	title            TEXT NOT NULL,
	author           TEXT,
	tags             TEXT,
	duration_seconds INTEGER DEFAULT 0,
	size_bytes       INTEGER DEFAULT 0,
	ext              TEXT,
	quality          TEXT,
	thumbnail_url    TEXT,
	preview_file_id  TEXT,
	preview_local    TEXT,
	preview_status   TEXT DEFAULT 'pending',
	views            INTEGER DEFAULT 0,
	favorites        INTEGER DEFAULT 0,
	comments         INTEGER DEFAULT 0,
	likes            INTEGER DEFAULT 0,
	dislikes         INTEGER DEFAULT 0,
	category         TEXT,
	hidden           INTEGER DEFAULT 0,
	tags_manual      INTEGER DEFAULT 0,
	badges           TEXT,
	description      TEXT,
	published_at     INTEGER NOT NULL,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL
)`); err != nil {
		t.Fatalf("create legacy videos table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	cat, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	var fileNameDefault string
	if err := cat.db.QueryRow(`SELECT COALESCE(file_name, '') FROM videos LIMIT 1`).Scan(&fileNameDefault); err != nil && err != sql.ErrNoRows {
		t.Fatalf("query migrated file_name column: %v", err)
	}
}

func TestSetManualVideoTagsRejectsUnknownLabels(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "普通标题",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}

	if err := cat.SetManualVideoTags(ctx, "video-1", []string{"不存在"}); err == nil {
		t.Fatal("SetManualVideoTags accepted an unknown tag label")
	}
}

func TestSetAutoVideoTagsDoesNotOverwriteManualVideoTags(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertVideo(ctx, &Video{
		ID:          "video-1",
		DriveID:     "drive",
		FileID:      "file-1",
		Title:       "清纯后入",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if _, err := cat.CreateTagAndClassify(ctx, "清纯", nil, "user"); err != nil {
		t.Fatalf("create user tag: %v", err)
	}
	if err := cat.SetManualVideoTags(ctx, "video-1", []string{"清纯"}); err != nil {
		t.Fatalf("set manual tags: %v", err)
	}

	if err := cat.SetAutoVideoTags(ctx, "video-1", []string{"后入"}); err != nil {
		t.Fatalf("set auto tags: %v", err)
	}

	got, err := cat.GetVideo(ctx, "video-1")
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if !sameStrings(got.Tags, []string{"清纯"}) {
		t.Fatalf("tags = %#v, want manual 清纯 only", got.Tags)
	}
}

func TestCreateTagAndClassifyMapsAVCodeLabelToAV(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	if _, err := cat.CreateTagAndClassify(ctx, "cc-1750027", nil, "user"); err != nil {
		t.Fatalf("create code tag: %v", err)
	}

	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	for _, tag := range tags {
		if tag.Label == "cc-1750027" {
			t.Fatal("created standalone AV code tag cc-1750027")
		}
	}
}

func TestLooksLikeCollectionTagRejectsAVCodes(t *testing.T) {
	cases := []string{
		"DASS-499-C",
		"dass-499-c",
		"ADN-778",
		"SONE-247-C",
		"JUQ-502-UC",
		"ABF-032",
		"SSIS-233",
		"MIDA-607",
		"cc-1750027",
		"FC2-PPV-74663555",
		"ADN-778-FHD(1)",
		"ADN-778-中文字幕",
		"[44x.me]idbd-786",
		"NTRH-018_FHD_CH",
		"390JAC-233",
	}
	for _, label := range cases {
		if LooksLikeCollectionTag(label) {
			t.Fatalf("LooksLikeCollectionTag(%q) = true, want false", label)
		}
	}
}

func TestMigrateCollapsesAVCodeTagsIntoAV(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	for _, seed := range []struct {
		id    string
		label string
	}{
		{id: "video-1", label: "cc-1750027"},
		{id: "video-2", label: "ADN-778-FHD(1)"},
		{id: "video-3", label: "[44x.me]idbd-786"},
		{id: "video-4", label: "390JAC-233"},
	} {
		if err := cat.UpsertVideo(ctx, &Video{
			ID:          seed.id,
			DriveID:     "drive",
			FileID:      seed.id,
			Title:       seed.label + " sample",
			Tags:        []string{seed.label},
			Category:    seed.label,
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed polluted video %s: %v", seed.label, err)
		}
	}

	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tags, err := cat.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	var sawAV bool
	polluted := map[string]bool{}
	for _, tag := range tags {
		if tag.Label == "AV" {
			sawAV = true
		}
		if tag.Label != "AV" && isAVCodePollutedLabel(tag.Label) {
			polluted[tag.Label] = true
		}
	}
	if !sawAV {
		t.Fatal("AV tag was not seeded")
	}
	if len(polluted) > 0 {
		t.Fatalf("AV code tags were not removed: %#v", polluted)
	}

	for _, id := range []string{"video-1", "video-2", "video-3", "video-4"} {
		got, err := cat.GetVideo(ctx, id)
		if err != nil {
			t.Fatalf("get video %s: %v", id, err)
		}
		if !sameStrings(got.Tags, []string{"AV"}) {
			t.Fatalf("%s tags = %#v, want AV", id, got.Tags)
		}
	}
}

func TestMigrateClearsVolatileOneDriveThumbnailURLs(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertDrive(ctx, &Drive{
		ID:        "onedrive-main",
		Kind:      "onedrive",
		Name:      "OneDrive",
		RootID:    "root",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed onedrive: %v", err)
	}

	videos := []*Video{
		{
			ID:           "onedrive-video",
			DriveID:      "onedrive-main",
			FileID:       "file-1",
			Title:        "OneDrive",
			ThumbnailURL: "https://westus21-mediap.svc.ms/transform/thumbnail?provider=spo&tempauth=expired",
		},
		{
			ID:           "local-thumb-video",
			DriveID:      "onedrive-main",
			FileID:       "file-2",
			Title:        "Local thumb",
			ThumbnailURL: "/p/thumb/local-thumb-video",
		},
		{
			ID:           "pikpak-video",
			DriveID:      "pikpak-main",
			FileID:       "file-3",
			Title:        "PikPak",
			ThumbnailURL: "https://sg-thumbnail-drive.mypikpak.net/v0/screenshot-thumbnails/demo",
		},
	}
	for _, v := range videos {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := cat.GetVideo(ctx, "onedrive-video")
	if err != nil {
		t.Fatalf("get onedrive video: %v", err)
	}
	if got.ThumbnailURL != "" {
		t.Fatalf("onedrive thumbnail = %q, want cleared", got.ThumbnailURL)
	}

	local, err := cat.GetVideo(ctx, "local-thumb-video")
	if err != nil {
		t.Fatalf("get local thumb video: %v", err)
	}
	if local.ThumbnailURL != "/p/thumb/local-thumb-video" {
		t.Fatalf("local thumbnail = %q, want preserved", local.ThumbnailURL)
	}

	pikpak, err := cat.GetVideo(ctx, "pikpak-video")
	if err != nil {
		t.Fatalf("get pikpak video: %v", err)
	}
	if pikpak.ThumbnailURL == "" {
		t.Fatal("pikpak thumbnail was cleared")
	}
}

func TestMigrateHidesZeroSizeVideosForKnownDrives(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	if err := cat.UpsertDrive(ctx, &Drive{
		ID:        "drive-main",
		Kind:      "onedrive",
		Name:      "OneDrive",
		RootID:    "root",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	for _, v := range []*Video{
		{ID: "empty-video", DriveID: "drive-main", FileID: "file-1", Title: "Empty", Size: 0},
		{ID: "normal-video", DriveID: "drive-main", FileID: "file-2", Title: "Normal", Size: 123},
		{ID: "orphan-empty-video", DriveID: "unknown-drive", FileID: "file-3", Title: "Orphan", Size: 0},
	} {
		v.PublishedAt = now
		v.CreatedAt = now
		v.UpdatedAt = now
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	if err := cat.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	empty, err := cat.GetVideo(ctx, "empty-video")
	if err != nil {
		t.Fatalf("get empty video: %v", err)
	}
	if !empty.Hidden {
		t.Fatal("empty video was not hidden")
	}

	normal, err := cat.GetVideo(ctx, "normal-video")
	if err != nil {
		t.Fatalf("get normal video: %v", err)
	}
	if normal.Hidden {
		t.Fatal("normal video was hidden")
	}

	orphan, err := cat.GetVideo(ctx, "orphan-empty-video")
	if err != nil {
		t.Fatalf("get orphan empty video: %v", err)
	}
	if orphan.Hidden {
		t.Fatal("orphan empty video without a known drive was hidden")
	}
}

func TestListVideosHidesDuplicateContentHashes(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	now := time.Now()
	for _, v := range []*Video{
		{
			ID:          "video-1",
			DriveID:     "drive",
			FileID:      "file-1",
			Title:       "First",
			ContentHash: "hash-same",
			PublishedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		{
			ID:          "video-2",
			DriveID:     "drive",
			FileID:      "file-2",
			Title:       "Second",
			ContentHash: "hash-same",
			PublishedAt: now.Add(time.Second),
			CreatedAt:   now.Add(time.Second),
			UpdatedAt:   now.Add(time.Second),
		},
	} {
		if err := cat.UpsertVideo(ctx, v); err != nil {
			t.Fatalf("seed video %s: %v", v.ID, err)
		}
	}

	items, total, err := cat.ListVideos(ctx, ListParams{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list videos: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("visible videos total=%d len=%d, want 1", total, len(items))
	}
	if items[0].ID != "video-1" {
		t.Fatalf("visible id = %q, want video-1", items[0].ID)
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
