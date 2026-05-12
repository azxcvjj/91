package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
)

type Config struct {
	FFmpegPath      string
	FFprobePath     string
	DurationSeconds int // 兼容旧配置；当前 teaser 每段固定 3 秒
	Width           int
	Segments        int    // 兼容旧配置；当前 30 秒及以上视频固定使用 4 段
	LocalDir        string // 本地兜底
	RemoteDir       string // 远端目录路径（相对盘根）
}

type Generator struct {
	cfg Config
}

type ThumbnailGenerator interface {
	Probe(ctx context.Context, link *drives.StreamLink) (float64, error)
	GenerateThumbnail(ctx context.Context, link *drives.StreamLink, videoID string, duration float64) (string, error)
}

type TeaserGenerator interface {
	Probe(ctx context.Context, link *drives.StreamLink) (float64, error)
	Generate(ctx context.Context, link *drives.StreamLink, duration float64) (string, error)
	MoveToLocal(tmpPath, videoID string) (string, error)
}

func New(cfg Config) *Generator {
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if cfg.FFprobePath == "" {
		cfg.FFprobePath = "ffprobe"
	}
	if cfg.DurationSeconds != 3 {
		cfg.DurationSeconds = 3
	}
	if cfg.Width == 0 {
		cfg.Width = 480
	}
	if cfg.Segments <= 0 {
		cfg.Segments = 3
	}
	return &Generator{cfg: cfg}
}

// --- 选段策略 ---

type teaserPlan struct {
	starts  []float64
	eachSec float64
}

func buildTeaserPlan(cfg Config, duration float64) teaserPlan {
	if cfg.DurationSeconds != 3 {
		cfg.DurationSeconds = 3
	}
	if cfg.Segments <= 0 {
		cfg.Segments = 3
	}

	segs := 1
	if duration > 0 && duration < 30 {
		segs = 3
	} else if duration >= 30 {
		segs = 4
	}

	eachSec := 3.0

	return teaserPlan{
		starts:  pickSegmentStarts(duration, segs, eachSec),
		eachSec: eachSec,
	}
}

// pickSegmentStarts 根据视频总时长选出 N 段起点秒数（按时间升序）
//
// 规则：
//   - duration < 30s → 最多 3 段；放不下完整 3 秒片段时丢弃对应片段
//   - 30s ≤ duration < 10min → 4 段：前段跳过片头、末段避开片尾
//   - duration ≥ 10min → 固定 4 段，按 20% ~ 80% 等距分布
func pickSegmentStarts(duration float64, n int, eachSec float64) []float64 {
	if n <= 0 {
		n = 1
	}
	if duration <= 0 {
		// 未知时长，用保守默认
		return []float64{10}
	}
	if duration < 30 {
		completeSegments := int(math.Floor(duration / eachSec))
		if completeSegments > n {
			completeSegments = n
		}
		if completeSegments <= 0 {
			return nil
		}
		usable := duration - eachSec
		first := math.Min(duration*0.1, usable)
		if completeSegments == 1 {
			return []float64{math.Max(0, first)}
		}
		starts := make([]float64, 0, completeSegments)
		step := (usable - first) / float64(completeSegments-1)
		for i := 0; i < completeSegments; i++ {
			starts = append(starts, first+step*float64(i))
		}
		return starts
	}

	// 余量：保证最后一段结束前留 1 秒，避免切到文件末尾
	usable := duration - eachSec - 1
	if usable < 0 {
		usable = 0
	}

	if duration < 600 {
		// 30s ~ 10min：20% 起，均匀分段
		starts := make([]float64, 0, n)
		// 保证第一段跳过片头（>= 5% 或 3s）
		firstMin := math.Max(3, duration*0.05)
		// 最后一段结束 <= 85%，避开结尾
		lastMax := duration * 0.85
		if lastMax < firstMin {
			lastMax = firstMin
		}
		if n == 1 {
			return []float64{duration * 0.25}
		}
		step := (lastMax - firstMin) / float64(n-1)
		for i := 0; i < n; i++ {
			s := firstMin + step*float64(i)
			if s > usable {
				s = usable
			}
			starts = append(starts, s)
		}
		return starts
	}

	// 长视频：按 20% / 50% / 80% 布置
	if n == 1 {
		return []float64{duration * 0.3}
	}
	starts := make([]float64, 0, n)
	pct := make([]float64, 0, n)
	// 均匀在 [0.2, 0.8] 区间取 N 个点
	lo, hi := 0.2, 0.8
	if n == 1 {
		pct = append(pct, 0.3)
	} else {
		step := (hi - lo) / float64(n-1)
		for i := 0; i < n; i++ {
			pct = append(pct, lo+step*float64(i))
		}
	}
	for _, p := range pct {
		s := duration * p
		if s > usable {
			s = usable
		}
		starts = append(starts, s)
	}
	return starts
}

// pickThumbnailOffset 选封面抽帧的时间点（秒）。独立于 teaser。
func pickThumbnailOffset(duration float64) float64 {
	if duration <= 0 {
		return 5
	}
	// 短视频从 30% 抽；长视频从 20% 抽，避开片头
	if duration < 60 {
		return math.Max(1, duration*0.3)
	}
	return math.Max(5, math.Min(duration*0.2, 120))
}

// --- 封面 ---

// GenerateThumbnail 抽一张 jpg 封面。偏移点由 duration 决定（独立于 teaser）。
func (g *Generator) GenerateThumbnail(ctx context.Context, link *drives.StreamLink, videoID string, duration float64) (string, error) {
	dir := filepath.Join(g.cfg.LocalDir, "thumbs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, videoID+".jpg")
	offset := pickThumbnailOffset(duration)

	ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-ss", fmt.Sprintf("%.2f", offset),
	}
	if h := buildHeaders(link.Headers); h != "" {
		args = append(args, "-headers", h)
	}
	args = append(args,
		"-i", link.URL,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", g.cfg.Width),
		"-q:v", "3",
		"-y", dst,
	)

	cmd := exec.CommandContext(ctx2, g.cfg.FFmpegPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(dst)
		return "", ffmpegCommandError("ffmpeg thumb", err, out)
	}
	if info, statErr := os.Stat(dst); statErr != nil || info.Size() == 0 {
		os.Remove(dst)
		return "", fmt.Errorf("ffmpeg thumb produced empty file, stderr: %s", string(out))
	}
	return dst, nil
}

// --- 时长 ---

// Probe 用 ffprobe 拿视频时长（秒，浮点）
func (g *Generator) Probe(ctx context.Context, link *drives.StreamLink) (float64, error) {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
	}
	if h := buildHeaders(link.Headers); h != "" {
		args = append(args, "-headers", h)
	}
	args = append(args, link.URL)

	cmd := exec.CommandContext(ctx2, g.cfg.FFprobePath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, ffmpegCommandError("ffprobe", err, out)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" || raw == "N/A" {
		return 0, nil
	}
	return strconv.ParseFloat(raw, 64)
}

// --- Teaser ---

// Generate 拉取 teaser 到本地临时文件，返回路径。
// 根据 Config.Segments 和视频时长决定是单段还是多段拼接。
func (g *Generator) Generate(ctx context.Context, link *drives.StreamLink, duration float64) (string, error) {
	if err := os.MkdirAll(g.cfg.LocalDir, 0o755); err != nil {
		return "", err
	}

	plan := buildTeaserPlan(g.cfg, duration)
	starts := plan.starts
	eachSec := plan.eachSec
	if len(starts) == 0 {
		return "", fmt.Errorf("video too short for %.0fs teaser segment", eachSec)
	}

	ctx2, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	// 用 ffmpeg 的 concat 滤镜一次输出：多个 -ss input 再 concat + fade
	tmp, err := os.CreateTemp(g.cfg.LocalDir, "teaser-*.mp4")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
	}
	headers := buildHeaders(link.Headers)

	// 每段独立 -ss + -i，精确 seek 重新解码保证拼接帧准
	for _, s := range starts {
		if headers != "" {
			args = append(args, "-headers", headers)
		}
		args = append(args,
			"-ss", fmt.Sprintf("%.2f", s),
			"-t", fmt.Sprintf("%.2f", eachSec),
			"-i", link.URL,
		)
	}

	if len(starts) == 1 {
		// 单段：无需 concat，直接缩放 + 无音
		args = append(args,
			"-an",
			"-vf", fmt.Sprintf("scale=%d:-2", g.cfg.Width),
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-crf", "28",
			"-movflags", "+faststart",
			"-y", tmpPath,
		)
	} else {
		// 多段：各段缩放 + 0.2s 黑场淡入淡出，concat 拼接
		// filter_complex: [0:v]scale,fade=in:0:5,fade=out:start=eachSec-0.2:d=0.2[v0]; ...; [v0][v1][v2]concat=n=3:v=1:a=0[v]
		fadeIn := 0.2
		fadeOutStart := eachSec - 0.2
		if fadeOutStart < 0 {
			fadeOutStart = 0
		}
		var filter strings.Builder
		for i := range starts {
			if i > 0 {
				filter.WriteString(";")
			}
			fmt.Fprintf(&filter,
				"[%d:v]scale=%d:-2,fade=t=in:st=0:d=%.2f,fade=t=out:st=%.2f:d=0.2[v%d]",
				i, g.cfg.Width, fadeIn, fadeOutStart, i)
		}
		filter.WriteString(";")
		for i := range starts {
			fmt.Fprintf(&filter, "[v%d]", i)
		}
		fmt.Fprintf(&filter, "concat=n=%d:v=1:a=0[v]", len(starts))

		args = append(args,
			"-filter_complex", filter.String(),
			"-map", "[v]",
			"-an",
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-crf", "28",
			"-movflags", "+faststart",
			"-y", tmpPath,
		)
	}

	cmd := exec.CommandContext(ctx2, g.cfg.FFmpegPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(tmpPath)
		return "", ffmpegCommandError("ffmpeg", err, out)
	}

	if info, statErr := os.Stat(tmpPath); statErr != nil || info.Size() == 0 {
		os.Remove(tmpPath)
		return "", fmt.Errorf("ffmpeg produced empty file, stderr: %s", string(out))
	}
	return tmpPath, nil
}

func ffmpegCommandError(tool string, err error, output []byte) error {
	msg := fmt.Sprintf("%s: %v, stderr: %s", tool, err, redactURLs(string(output)))
	wrapped := errors.New(msg)
	if ffmpegOutputLooksRateLimited(output) {
		return &drives.RateLimitError{
			Provider: "media source",
			Err:      wrapped,
		}
	}
	return wrapped
}

func redactURLs(text string) string {
	fields := strings.Fields(text)
	for i, field := range fields {
		if strings.HasPrefix(field, "http://") || strings.HasPrefix(field, "https://") {
			suffix := ""
			for len(field) > 0 {
				last := field[len(field)-1]
				if last != '.' && last != ',' && last != ';' && last != ')' {
					break
				}
				suffix = string(last) + suffix
				field = field[:len(field)-1]
			}
			fields[i] = "https://<redacted>" + suffix
		}
	}
	return strings.Join(fields, " ")
}

func ffmpegOutputLooksRateLimited(output []byte) bool {
	text := strings.ToLower(string(output))
	if !strings.Contains(text, "429") {
		return false
	}
	return strings.Contains(text, "too many requests") ||
		strings.Contains(text, "rate limit") ||
		strings.Contains(text, "rate-limit") ||
		strings.Contains(text, "server returned 429")
}

// --- 本地落盘 ---

// MoveToLocal 把临时文件改名到稳定位置，返回最终路径
func (g *Generator) MoveToLocal(tmpPath, videoID string) (string, error) {
	dst := filepath.Join(g.cfg.LocalDir, videoID+".mp4")
	if err := os.Rename(tmpPath, dst); err != nil {
		// 跨盘 rename 可能失败，fallback 到 copy
		if cerr := copyFile(tmpPath, dst); cerr != nil {
			return "", cerr
		}
		_ = os.Remove(tmpPath)
	}
	return dst, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// --- Worker ---

type Worker struct {
	Gen       TeaserGenerator
	Catalog   *catalog.Catalog
	Drive     drives.Drive
	RemoteDir string
	ch        chan *catalog.Video

	RateLimitCooldown time.Duration
	rateLimit         rateLimitState
}

func NewWorker(gen TeaserGenerator, cat *catalog.Catalog, drv drives.Drive, remoteDir string) *Worker {
	return &Worker{
		Gen:       gen,
		Catalog:   cat,
		Drive:     drv,
		RemoteDir: remoteDir,
		ch:        make(chan *catalog.Video, 4096),
	}
}

func (w *Worker) Enqueue(v *catalog.Video) bool {
	if v == nil {
		return false
	}
	select {
	case w.ch <- v:
		return true
	default:
		return false
	}
}

func (w *Worker) EnqueueBlocking(ctx context.Context, v *catalog.Video) bool {
	if v == nil {
		return false
	}
	select {
	case w.ch <- v:
		return true
	case <-ctx.Done():
		return false
	}
}

type ThumbWorker struct {
	Gen     ThumbnailGenerator
	Catalog *catalog.Catalog
	Drive   drives.Drive
	ch      chan *catalog.Video

	RateLimitCooldown time.Duration
	rateLimit         rateLimitState
}

const defaultRateLimitCooldown = 30 * time.Minute

type rateLimitState struct {
	mu          sync.Mutex
	until       time.Time
	lastSkipLog time.Time
}

func (s *rateLimitState) active(now time.Time) (time.Time, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until.IsZero() || !now.Before(s.until) {
		return time.Time{}, false, false
	}
	shouldLog := s.lastSkipLog.IsZero() || now.Sub(s.lastSkipLog) >= 5*time.Minute
	if shouldLog {
		s.lastSkipLog = now
	}
	return s.until, true, shouldLog
}

func (s *rateLimitState) pause(now time.Time, d time.Duration) time.Time {
	if d <= 0 {
		d = defaultRateLimitCooldown
	}
	until := now.Add(d)
	s.mu.Lock()
	if until.After(s.until) {
		s.until = until
	} else {
		until = s.until
	}
	s.lastSkipLog = time.Time{}
	s.mu.Unlock()
	return until
}

func NewThumbWorker(gen ThumbnailGenerator, cat *catalog.Catalog, drv drives.Drive) *ThumbWorker {
	return &ThumbWorker{
		Gen:     gen,
		Catalog: cat,
		Drive:   drv,
		ch:      make(chan *catalog.Video, 4096),
	}
}

func (w *ThumbWorker) Enqueue(v *catalog.Video) bool {
	if v == nil {
		return false
	}
	select {
	case w.ch <- v:
		return true
	default:
		return false
	}
}

func (w *ThumbWorker) EnqueueBlocking(ctx context.Context, v *catalog.Video) bool {
	if v == nil {
		return false
	}
	select {
	case w.ch <- v:
		return true
	case <-ctx.Done():
		return false
	}
}

// Run 阻塞运行直到 ctx 取消
func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-w.ch:
			w.process(ctx, v)
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

// Run 阻塞运行直到 ctx 取消
func (w *ThumbWorker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-w.ch:
			w.process(ctx, v)
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

func (w *Worker) skipIfRateLimited(v *catalog.Video) bool {
	until, ok, shouldLog := w.rateLimit.active(time.Now())
	if !ok {
		return false
	}
	if shouldLog {
		log.Printf("[preview] drive=%s rate-limited until=%s; skip queued videos and keep them pending", w.Drive.ID(), until.Format(time.RFC3339))
	}
	return true
}

func (w *Worker) pauseForRateLimit(err error, step, title string) bool {
	retryAfter, ok := drives.RateLimitRetryAfter(err)
	if !ok {
		return false
	}
	if retryAfter <= 0 {
		retryAfter = w.RateLimitCooldown
	}
	until := w.rateLimit.pause(time.Now(), retryAfter)
	log.Printf("[preview] drive=%s rate-limited until=%s step=%s video=%s: %v", w.Drive.ID(), until.Format(time.RFC3339), step, title, err)
	return true
}

func (w *ThumbWorker) skipIfRateLimited(v *catalog.Video) bool {
	until, ok, shouldLog := w.rateLimit.active(time.Now())
	if !ok {
		return false
	}
	if shouldLog {
		log.Printf("[thumb] drive=%s rate-limited until=%s; skip queued thumbnails and keep them pending", w.Drive.ID(), until.Format(time.RFC3339))
	}
	return true
}

func (w *ThumbWorker) pauseForRateLimit(err error, step, title string) bool {
	retryAfter, ok := drives.RateLimitRetryAfter(err)
	if !ok {
		return false
	}
	if retryAfter <= 0 {
		retryAfter = w.RateLimitCooldown
	}
	until := w.rateLimit.pause(time.Now(), retryAfter)
	log.Printf("[thumb] drive=%s rate-limited until=%s step=%s video=%s: %v", w.Drive.ID(), until.Format(time.RFC3339), step, title, err)
	return true
}

func (w *ThumbWorker) process(ctx context.Context, v *catalog.Video) {
	if w.skipIfRateLimited(v) {
		return
	}
	link, err := w.Drive.StreamURL(ctx, v.FileID)
	if err != nil {
		if localLink, ok := localPreviewLink(v); ok {
			link = localLink
		} else {
			if w.pauseForRateLimit(err, "streamURL", v.Title) {
				return
			}
			log.Printf("[thumb] streamURL %s: %v", v.Title, err)
			return
		}
	}

	if err := w.generateThumbnailFromLink(ctx, v, link); err != nil {
		if localLink, ok := localPreviewLink(v); ok && link.URL != localLink.URL {
			if localErr := w.generateThumbnailFromLink(ctx, v, localLink); localErr == nil {
				return
			}
		}
		if w.pauseForRateLimit(err, "generate", v.Title) {
			return
		}
		log.Printf("[thumb] generate %s: %v", v.Title, err)
		return
	}
}

func (w *ThumbWorker) generateThumbnailFromLink(ctx context.Context, v *catalog.Video, link *drives.StreamLink) error {
	duration := thumbnailDurationHint(v, link)
	if duration <= 0 {
		if dur, err := w.Gen.Probe(ctx, link); err == nil && dur > 0 {
			duration = dur
			_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
				DurationSeconds: int(dur),
			})
		} else if err != nil {
			return err
		}
	}

	if _, err := w.Gen.GenerateThumbnail(ctx, link, v.ID, duration); err != nil {
		return err
	}
	_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
		ThumbnailURL: "/p/thumb/" + v.ID,
	})
	log.Printf("[thumb] ready %s", v.Title)
	return nil
}

func thumbnailDurationHint(v *catalog.Video, link *drives.StreamLink) float64 {
	if link != nil && v.PreviewLocal != "" && filepath.Clean(link.URL) == filepath.Clean(v.PreviewLocal) {
		return 0
	}
	return float64(v.DurationSeconds)
}

func localPreviewLink(v *catalog.Video) (*drives.StreamLink, bool) {
	if v.PreviewLocal == "" {
		return nil, false
	}
	clean := filepath.Clean(v.PreviewLocal)
	info, err := os.Stat(clean)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return nil, false
	}
	return &drives.StreamLink{URL: clean}, true
}

func (w *Worker) process(ctx context.Context, v *catalog.Video) {
	if w.skipIfRateLimited(v) {
		return
	}
	link, err := w.Drive.StreamURL(ctx, v.FileID)
	if err != nil {
		if w.pauseForRateLimit(err, "streamURL", v.Title) {
			return
		}
		log.Printf("[preview] streamURL %s: %v", v.Title, err)
		w.Catalog.UpdatePreview(ctx, v.ID, "", "", "failed")
		return
	}

	// 1) 探时长（失败用 0 继续）
	duration := float64(v.DurationSeconds)
	if duration <= 0 {
		if dur, err := w.Gen.Probe(ctx, link); err == nil && dur > 0 {
			duration = dur
			_ = w.Catalog.UpdateVideoMeta(ctx, v.ID, catalog.VideoMetaPatch{
				DurationSeconds: int(dur),
			})
		} else if err != nil && w.pauseForRateLimit(err, "probe", v.Title) {
			return
		}
	}

	// 2) teaser
	tmp, err := w.Gen.Generate(ctx, link, duration)
	if err != nil {
		if w.pauseForRateLimit(err, "generate", v.Title) {
			return
		}
		log.Printf("[preview] generate %s: %v", v.Title, err)
		w.Catalog.UpdatePreview(ctx, v.ID, "", "", "failed")
		return
	}
	local, err := w.Gen.MoveToLocal(tmp, v.ID)
	if err != nil {
		log.Printf("[preview] move %s: %v", v.Title, err)
		w.Catalog.UpdatePreview(ctx, v.ID, "", "", "failed")
		return
	}

	previewFileID := ""
	if w.RemoteDir != "" {
		if fid, uerr := w.uploadToDrive(ctx, v.ID, local); uerr == nil {
			previewFileID = fid
		} else {
			if w.pauseForRateLimit(uerr, "upload", v.Title) {
				log.Printf("[preview] upload %s: %v (local only; drive cooling down)", v.Title, uerr)
			} else {
				log.Printf("[preview] upload %s: %v (local only)", v.Title, uerr)
			}
		}
	}
	removePreviousLocalTeaser(v.PreviewLocal, local)
	w.Catalog.UpdatePreview(ctx, v.ID, previewFileID, local, "ready")
	log.Printf("[preview] ready %s (duration=%.1fs)", v.Title, duration)
}

func removePreviousLocalTeaser(previous, current string) {
	if previous == "" {
		return
	}
	if filepath.Clean(previous) == filepath.Clean(current) {
		return
	}
	if err := os.Remove(previous); err != nil && !os.IsNotExist(err) {
		log.Printf("[preview] remove old local teaser %s: %v", previous, err)
	}
}

func (w *Worker) uploadToDrive(ctx context.Context, videoID, localPath string) (string, error) {
	parentID, err := w.Drive.EnsureDir(ctx, w.RemoteDir)
	if err != nil {
		return "", err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	return w.Drive.Upload(ctx, parentID, videoID+".mp4", f, stat.Size())
}

// --- utils ---

func buildHeaders(h map[string][]string) string {
	if len(h) == 0 {
		return ""
	}
	var sb strings.Builder
	for k, vs := range h {
		for _, v := range vs {
			sb.WriteString(k)
			sb.WriteString(": ")
			sb.WriteString(v)
			sb.WriteString("\r\n")
		}
	}
	return sb.String()
}
