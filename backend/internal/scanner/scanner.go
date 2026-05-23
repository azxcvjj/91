package scanner

import (
	"context"
	"fmt"
	"log"
	"path"
	"strings"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
)

type Scanner struct {
	Catalog  *catalog.Catalog
	Drive    drives.Drive
	Exts     map[string]bool
	MaxDepth int
	// 回调：新视频被加入后触发 teaser 生成
	OnNewVideo func(v *catalog.Video)
}

func New(cat *catalog.Catalog, drv drives.Drive, exts []string, maxDepth int, onNew func(v *catalog.Video)) *Scanner {
	m := make(map[string]bool, len(exts))
	for _, e := range exts {
		m[strings.ToLower(e)] = true
	}
	if maxDepth == 0 {
		maxDepth = 5
	}
	return &Scanner{
		Catalog:    cat,
		Drive:      drv,
		Exts:       m,
		MaxDepth:   maxDepth,
		OnNewVideo: onNew,
	}
}

type Stats struct {
	Scanned         int
	Added           int
	Errors          int
	SeenFileIDs     map[string]struct{}
	VisitedDirIDs   map[string]struct{}
	ExcludedFileIDs map[string]struct{}
}

// Run 从 Drive.RootID 开始扫描
func (s *Scanner) Run(ctx context.Context, startDirID string) (Stats, error) {
	if startDirID == "" {
		startDirID = s.Drive.RootID()
	}
	stats := Stats{
		SeenFileIDs:     make(map[string]struct{}),
		VisitedDirIDs:   make(map[string]struct{}),
		ExcludedFileIDs: make(map[string]struct{}),
	}
	if err := s.walk(ctx, startDirID, "", 0, &stats); err != nil {
		return stats, err
	}
	return stats, nil
}

func (s *Scanner) walk(ctx context.Context, dirID, dirName string, depth int, stats *Stats) error {
	if depth >= s.MaxDepth {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stats.VisitedDirIDs[dirID] = struct{}{}

	entries, err := s.Drive.List(ctx, dirID)
	if err != nil {
		return fmt.Errorf("list %s: %w", dirID, err)
	}

	for _, e := range entries {
		if e.IsDir {
			// 跳过 previews 目录，避免扫到自己生成的 teaser
			if strings.EqualFold(e.Name, "previews") {
				continue
			}
			if s.shouldExcludeDir(e.Name) {
				if err := s.collectExcludedFiles(ctx, e.ID, depth+1, stats); err != nil {
					stats.Errors++
					log.Printf("[scanner] exclude %s error: %v", e.Name, err)
				}
				continue
			}
			if err := s.walk(ctx, e.ID, e.Name, depth+1, stats); err != nil {
				stats.Errors++
				log.Printf("[scanner] walk %s error: %v", e.Name, err)
			}
			continue
		}

		stats.Scanned++
		ext := strings.ToLower(path.Ext(e.Name))
		if !s.Exts[ext] {
			continue
		}
		if e.Size <= 0 {
			continue
		}
		stats.SeenFileIDs[e.ID] = struct{}{}

		id := s.Drive.Kind() + "-" + s.Drive.ID() + "-" + e.ID
		parsed := Parse(e.Name)
		if parsed.Title == "" {
			parsed.Title = strings.TrimSuffix(e.Name, ext)
		}
		tags := parsed.Tags
		if matched, err := s.Catalog.MatchTags(ctx, e.Name+" "+dirName+" "+parsed.Author); err == nil {
			tags = mergeTags(tags, matched)
		}
		if label, ok, err := s.Catalog.EnsureCollectionTag(ctx, dirName); err == nil && ok {
			tags = mergeTags(tags, []string{label})
		}

		existing, _ := s.Catalog.GetVideo(ctx, id)
		if existing != nil {
			patch := catalog.VideoMetaPatch{}
			if e.Hash != "" && existing.ContentHash == "" {
				patch.ContentHash = e.Hash
				existing.ContentHash = e.Hash
			}
			if e.Name != "" && existing.FileName == "" {
				patch.FileName = e.Name
				existing.FileName = e.Name
			}
			// 已存在但轻量元数据空缺时，顺便补齐。
			if existing.Category == "" && dirName != "" {
				patch.Category = dirName
			}
			if existing.ThumbnailURL == "" && e.ThumbnailURL != "" {
				patch.ThumbnailURL = e.ThumbnailURL
			}
			if patch.Category != "" || patch.ThumbnailURL != "" || patch.ContentHash != "" || patch.FileName != "" {
				_ = s.Catalog.UpdateVideoMeta(ctx, id, patch)
			}
			if dup := s.findDuplicate(ctx, e.Hash, e.Name, e.Size, id); dup != nil {
				s.backfillDuplicateThumbnail(ctx, dup, e.ThumbnailURL)
				continue
			}
			if !sameTags(existing.Tags, tags) {
				_ = s.Catalog.SetAutoVideoTags(ctx, id, tags)
			}
			continue
		}

		if dup := s.findDuplicate(ctx, e.Hash, e.Name, e.Size, id); dup != nil {
			s.backfillDuplicateThumbnail(ctx, dup, e.ThumbnailURL)
			continue
		}

		now := time.Now()
		v := &catalog.Video{
			ID:            id,
			DriveID:       s.Drive.ID(),
			FileID:        e.ID,
			FileName:      e.Name,
			ContentHash:   e.Hash,
			ParentID:      e.ParentID,
			Title:         parsed.Title,
			Author:        parsed.Author,
			Tags:          tags,
			Ext:           strings.TrimPrefix(ext, "."),
			Quality:       "HD",
			Size:          e.Size,
			ThumbnailURL:  e.ThumbnailURL,
			PreviewStatus: "pending",
			Category:      dirName,
			PublishedAt:   now,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := s.Catalog.UpsertVideo(ctx, v); err != nil {
			log.Printf("[scanner] upsert %s error: %v", v.Title, err)
			continue
		}
		stats.Added++
		if s.OnNewVideo != nil {
			s.OnNewVideo(v)
		}
	}
	return nil
}

func (s *Scanner) shouldExcludeDir(name string) bool {
	return s.Drive != nil &&
		s.Drive.Kind() == "p115" &&
		strings.TrimSpace(name) == "影视"
}

func (s *Scanner) collectExcludedFiles(ctx context.Context, dirID string, depth int, stats *Stats) error {
	if depth >= s.MaxDepth {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := s.Drive.List(ctx, dirID)
	if err != nil {
		return fmt.Errorf("list excluded %s: %w", dirID, err)
	}
	for _, e := range entries {
		if e.IsDir {
			if err := s.collectExcludedFiles(ctx, e.ID, depth+1, stats); err != nil {
				return err
			}
			continue
		}
		if e.ID != "" {
			stats.ExcludedFileIDs[e.ID] = struct{}{}
		}
	}
	return nil
}

func (s *Scanner) findDuplicate(ctx context.Context, hash, fileName string, size int64, currentID string) *catalog.Video {
	if dup := s.findDuplicateByHash(ctx, hash, currentID); dup != nil {
		return dup
	}
	return s.findDuplicateByFileSignature(ctx, fileName, size, currentID)
}

func (s *Scanner) findDuplicateByHash(ctx context.Context, hash, currentID string) *catalog.Video {
	if hash == "" {
		return nil
	}
	dup, err := s.Catalog.FindVideoByContentHash(ctx, hash)
	if err != nil || dup == nil || dup.ID == currentID {
		return nil
	}
	return dup
}

func (s *Scanner) findDuplicateByFileSignature(ctx context.Context, fileName string, size int64, currentID string) *catalog.Video {
	if fileName == "" || size <= 0 {
		return nil
	}
	dup, err := s.Catalog.FindVideoByFileSignature(ctx, fileName, size)
	if err != nil || dup == nil || dup.ID == currentID {
		return nil
	}
	return dup
}

func (s *Scanner) backfillDuplicateThumbnail(ctx context.Context, canonical *catalog.Video, thumbnailURL string) {
	if canonical.ThumbnailURL != "" || thumbnailURL == "" {
		return
	}
	_ = s.Catalog.UpdateVideoMeta(ctx, canonical.ID, catalog.VideoMetaPatch{ThumbnailURL: thumbnailURL})
}

func sameTags(a, b []string) bool {
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

func mergeTags(lists ...[]string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, list := range lists {
		for _, tag := range list {
			if tag == "" || seen[tag] {
				continue
			}
			seen[tag] = true
			out = append(out, tag)
		}
	}
	return out
}
