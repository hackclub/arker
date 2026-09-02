package workers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

const videoMetadataBackfillQueue = "video_metadata_backfill"

const (
	maxCapturedMHTMLScanBytes = 64 << 20
	maxCapturedHTMLBytes      = 16 << 20
)

// Contract repair is one metadata-only extractor request. Healthy platforms
// finish it in seconds; channel/playlist URLs and retired posts can otherwise
// occupy one of the deliberately scarce backfill workers for the full ten
// minute River job budget. Cap only the lean request and let River's second
// attempt handle transient slowness.
const videoContractMetadataRefreshTimeout = 2 * time.Minute

type VideoMetadataBackfillJobArgs struct {
	Identity string `json:"identity"`
	URL      string `json:"url"`
	ShortID  string `json:"short_id"`
	Version  int    `json:"version"`
}

func (VideoMetadataBackfillJobArgs) Kind() string { return "video_metadata_backfill" }

// VideoContractMetadataProvider is the paid provider's metadata-only path.
// It is deliberately narrower than an Archiver: historical media bytes are
// already safe in storage, so a provider fallback must buy only the post
// record (or a page resolution), never download the video a second time.
type VideoContractMetadataProvider interface {
	SupportsStoredVideoMetadata(url string) bool
	RefreshStoredVideoMetadata(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint, media archivers.VideoMedia) (archivers.Result, error)
}

type VideoMetadataBackfillWorker struct {
	river.WorkerDefaults[VideoMetadataBackfillJobArgs]
	storage   storage.Storage
	db        *gorm.DB
	refresher archivers.VideoMetadataRefresher
	provider  VideoContractMetadataProvider
}

func NewVideoMetadataBackfillWorker(store storage.Storage, db *gorm.DB, refresher archivers.VideoMetadataRefresher, providers ...VideoContractMetadataProvider) *VideoMetadataBackfillWorker {
	worker := &VideoMetadataBackfillWorker{storage: store, db: db, refresher: refresher}
	if len(providers) > 0 {
		worker.provider = providers[0]
	}
	return worker
}

func (w *VideoMetadataBackfillWorker) Work(ctx context.Context, job *river.Job[VideoMetadataBackfillJobArgs]) error {
	jobCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return w.generateAttempt(jobCtx, job.Args, job.Attempt >= job.MaxAttempts)
}

func (w *VideoMetadataBackfillWorker) generate(ctx context.Context, args VideoMetadataBackfillJobArgs) error {
	return w.generateAttempt(ctx, args, false)
}

func (w *VideoMetadataBackfillWorker) generateAttempt(ctx context.Context, args VideoMetadataBackfillJobArgs, finalAttempt bool) error {
	items, err := w.groupItems(args.Identity)
	if err != nil {
		return err
	}
	targets := make([]models.ArchiveItem, 0, len(items))
	var source *models.ArchiveItem
	for i := range items {
		item := items[i]
		if item.MetadataKey != "" && item.RawMetadataKey != "" {
			if source == nil {
				source = &items[i]
			}
			continue
		}
		targets = append(targets, item)
	}
	if len(targets) == 0 {
		return nil
	}
	if source != nil {
		return w.share(source, targets)
	}
	if w.refresher == nil {
		return errors.New("video metadata refresher is not configured")
	}
	representative := &targets[0]
	var logs bytes.Buffer
	media := archivers.VideoMedia{
		Extension: representative.Extension,
		SizeBytes: representative.FileSize,
	}
	var result archivers.Result
	var capturedErr error
	if capturedErr = ctx.Err(); capturedErr == nil {
		fmt.Fprintf(&logs, "Checking the capture's MHTML metadata before a live extractor request\n")
		result, capturedErr = w.refreshFromCapturedMHTML(ctx, args.URL, items, media, &logs)
	}
	if capturedErr != nil {
		// The first attempt has already exercised the live extractor. On the
		// final attempt, go straight to the independent metadata-only provider
		// when one supports this URL instead of repeating the same expensive
		// request. Captured MHTML remains first because it is immutable, free,
		// and historically faithful.
		if finalAttempt && ctx.Err() == nil && w.provider != nil && w.provider.SupportsStoredVideoMetadata(args.URL) {
			fmt.Fprintf(&logs, "Captured MHTML metadata was unavailable (%v); attempting metadata-only provider fallback\n", capturedErr)
			result, err = w.provider.RefreshStoredVideoMetadata(ctx, args.URL, &logs, w.db, representative.ID, media)
			if err != nil {
				providerErr := err
				fmt.Fprintf(&logs, "Metadata-only provider failed (%v); checking the original capture probe log\n", providerErr)
				result, err = w.refreshFromArchivedProbeLogs(ctx, args.URL, items, media, &logs)
				if err != nil {
					return fmt.Errorf("refresh video metadata for %s: captured MHTML failed (%v); provider failed (%v); capture log failed: %w", args.ShortID, capturedErr, providerErr, err)
				}
			}
		} else {
			fmt.Fprintf(&logs, "Captured MHTML metadata was unavailable (%v); trying the native metadata extractor\n", capturedErr)
			if lean, ok := w.refresher.(archivers.VideoContractMetadataRefresher); ok {
				refreshCtx, cancel := context.WithTimeout(ctx, videoContractMetadataRefreshTimeout)
				result, err = lean.RefreshVideoContractMetadata(refreshCtx, args.URL, &logs, media)
				cancel()
			} else {
				result, err = w.refresher.RefreshVideoMetadata(ctx, args.URL, &logs, media)
			}
			if err != nil {
				if finalAttempt {
					nativeErr := err
					fmt.Fprintf(&logs, "Native metadata extractor failed (%v); checking the original capture probe log\n", nativeErr)
					result, err = w.refreshFromArchivedProbeLogs(ctx, args.URL, items, media, &logs)
					if err != nil {
						return fmt.Errorf("refresh video metadata for %s: captured MHTML failed (%v); native failed (%v); capture log failed: %w", args.ShortID, capturedErr, nativeErr, err)
					}
				} else {
					return fmt.Errorf("refresh video metadata for %s: %w", args.ShortID, err)
				}
			}
		}
	}
	if result.Bundle != nil {
		defer result.Bundle.Cleanup()
	}
	keyBase := fmt.Sprintf("%s/%s-%s-metadata-backfill", args.ShortID, utils.ArchiveTypeYtDlp, uploadNonce())
	if err := saveRefreshedArchiveResult(ctx, result, keyBase, representative, w.storage, w.db, representative, &logs); err != nil {
		return err
	}
	if err := w.db.First(representative, representative.ID).Error; err != nil {
		return err
	}
	return w.share(representative, targets)
}

// refreshFromArchivedProbeLogs uses only the three values written by the
// successful pre-download yt-dlp probe. Querying marker-bearing chunks keeps
// this fallback cheap even when an old item's diagnostic log is very large.
func (w *VideoMetadataBackfillWorker) refreshFromArchivedProbeLogs(ctx context.Context, sourceURL string, videos []models.ArchiveItem, media archivers.VideoMedia, logWriter io.Writer) (archivers.Result, error) {
	itemIDs := make([]uint, 0, len(videos))
	for i := range videos {
		itemIDs = append(itemIDs, videos[i].ID)
	}
	if len(itemIDs) == 0 {
		return archivers.Result{}, errors.New("video group has no archive items")
	}
	var rows []models.ArchiveItemLog
	if err := w.db.Where("archive_item_id IN ? AND chunk LIKE ?", itemIDs, "%Video info:%").Order("id DESC").Find(&rows).Error; err != nil {
		return archivers.Result{}, fmt.Errorf("load archived probe logs: %w", err)
	}
	var lastErr error
	for _, row := range rows {
		if ctx.Err() != nil {
			return archivers.Result{}, ctx.Err()
		}
		metadata, raw, err := archivers.BuildArchivedProbeVideoArtifacts(row.Chunk, sourceURL, media, row.CreatedAt)
		if err != nil {
			lastErr = err
			continue
		}
		fmt.Fprintf(logWriter, "Recovered historical post metadata from capture probe log chunk %d\n", row.ID)
		return archivers.Result{Extension: media.Extension, ContentType: media.ContentType, Source: models.ArchiveSourceNative, Metadata: metadata, RawMetadata: raw, Completeness: archivers.CompletenessComplete}, nil
	}
	for i := range videos {
		if !strings.Contains(videos[i].Logs, "Video info:") {
			continue
		}
		capturedAt := videos[i].UpdatedAt
		if capturedAt.IsZero() {
			capturedAt = videos[i].CreatedAt
		}
		metadata, raw, err := archivers.BuildArchivedProbeVideoArtifacts(videos[i].Logs, sourceURL, media, capturedAt)
		if err != nil {
			lastErr = err
			continue
		}
		fmt.Fprintf(logWriter, "Recovered historical post metadata from legacy capture probe log\n")
		return archivers.Result{Extension: media.Extension, ContentType: media.ContentType, Source: models.ArchiveSourceNative, Metadata: metadata, RawMetadata: raw, Completeness: archivers.CompletenessComplete}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no archived probe log contains usable metadata")
	}
	return archivers.Result{}, lastErr
}

// refreshFromCapturedMHTML recovers the post facts captured alongside the
// video before asking a paid live provider. This is both cheaper and more
// historically faithful: a post deleted today can still have its title,
// author, publication time, and schema.org duration in the old page snapshot.
func (w *VideoMetadataBackfillWorker) refreshFromCapturedMHTML(ctx context.Context, sourceURL string, videos []models.ArchiveItem, media archivers.VideoMedia, logWriter io.Writer) (archivers.Result, error) {
	captureIDs := make([]uint, 0, len(videos))
	seen := make(map[uint]bool, len(videos))
	for i := range videos {
		if !seen[videos[i].CaptureID] {
			seen[videos[i].CaptureID] = true
			captureIDs = append(captureIDs, videos[i].CaptureID)
		}
	}
	if len(captureIDs) == 0 {
		return archivers.Result{}, errors.New("video group has no captures")
	}
	var pages []models.ArchiveItem
	if err := w.db.Where("capture_id IN ? AND type = ? AND status = ? AND storage_key <> ''", captureIDs, utils.ArchiveTypeMHTML, "completed").
		Order("updated_at DESC").Find(&pages).Error; err != nil {
		return archivers.Result{}, fmt.Errorf("load sibling MHTML items: %w", err)
	}
	var lastErr error
	for i := range pages {
		if ctx.Err() != nil {
			return archivers.Result{}, ctx.Err()
		}
		reader, err := w.storage.Reader(pages[i].StorageKey)
		if err != nil {
			lastErr = err
			continue
		}
		htmlData, extractErr := utils.ExtractMHTMLHTML(io.LimitReader(reader, maxCapturedMHTMLScanBytes), maxCapturedHTMLBytes)
		closeErr := reader.Close()
		if extractErr != nil {
			lastErr = extractErr
			continue
		}
		if closeErr != nil {
			lastErr = closeErr
			continue
		}
		capturedAt := pages[i].UpdatedAt
		if capturedAt.IsZero() {
			capturedAt = pages[i].CreatedAt
		}
		metadata, raw, buildErr := archivers.BuildCapturedHTMLVideoArtifacts(htmlData, sourceURL, media, capturedAt)
		if buildErr != nil {
			lastErr = buildErr
			continue
		}
		fmt.Fprintf(logWriter, "Recovered historical post metadata from sibling MHTML object %s\n", pages[i].StorageKey)
		return archivers.Result{
			Extension:    media.Extension,
			ContentType:  media.ContentType,
			Source:       models.ArchiveSourceNative,
			Metadata:     metadata,
			RawMetadata:  raw,
			Completeness: archivers.CompletenessComplete,
		}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no completed sibling MHTML archive")
	}
	return archivers.Result{}, lastErr
}

func (w *VideoMetadataBackfillWorker) groupItems(identity string) ([]models.ArchiveItem, error) {
	var items []models.ArchiveItem
	err := w.db.Model(&models.ArchiveItem{}).
		Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Joins("JOIN archived_urls ON archived_urls.id = captures.archived_url_id").
		Where("archive_items.type IN ? AND archive_items.status = ?", utils.ArchiveTypeMatchValues(utils.ArchiveTypeYtDlp), "completed").
		Where("archive_items.deleted_at IS NULL AND captures.deleted_at IS NULL AND archived_urls.deleted_at IS NULL").
		Where("COALESCE(NULLIF(archived_urls.canonical_url, ''), archived_urls.original) = ?", identity).
		Order("archive_items.id DESC").Find(&items).Error
	return items, err
}

func (w *VideoMetadataBackfillWorker) share(source *models.ArchiveItem, targets []models.ArchiveItem) error {
	ids := make([]uint, 0, len(targets))
	for i := range targets {
		if targets[i].MetadataKey == "" || targets[i].RawMetadataKey == "" {
			ids = append(ids, targets[i].ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	updates := map[string]interface{}{
		"metadata_key": source.MetadataKey, "raw_metadata_key": source.RawMetadataKey,
	}
	if source.Completeness != "" {
		updates["completeness"] = source.Completeness
	}
	return w.db.Model(&models.ArchiveItem{}).Where("id IN ?", ids).Updates(updates).Error
}

type VideoMetadataBackfillOptions struct {
	DryRun bool
	Limit  int
}

type VideoMetadataBackfillSummary struct {
	RunID        string `json:"run_id"`
	TargetItems  int    `json:"target_items"`
	TargetGroups int    `json:"target_groups"`
	Enqueued     int    `json:"enqueued"`
	DryRun       bool   `json:"dry_run"`
}

type videoMetadataCandidate struct {
	ItemID, CaptureID           uint
	ShortID, Original, Identity string
}

func EnqueueVideoMetadataBackfill(ctx context.Context, db *gorm.DB, client *river.Client[pgx.Tx], opts VideoMetadataBackfillOptions) (VideoMetadataBackfillSummary, error) {
	now := time.Now().UTC()
	summary := VideoMetadataBackfillSummary{RunID: now.Format("20060102T150405Z"), DryRun: opts.DryRun}
	var rows []videoMetadataCandidate
	err := db.Table("archive_items").Select(`archive_items.id AS item_id, archive_items.capture_id, captures.short_id,
		archived_urls.original, COALESCE(NULLIF(archived_urls.canonical_url, ''), archived_urls.original) AS identity`).
		Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Joins("JOIN archived_urls ON archived_urls.id = captures.archived_url_id").
		Where("archive_items.type IN ? AND archive_items.status = ?", utils.ArchiveTypeMatchValues(utils.ArchiveTypeYtDlp), "completed").
		Where("archive_items.deleted_at IS NULL AND captures.deleted_at IS NULL AND archived_urls.deleted_at IS NULL").
		Where("archive_items.metadata_key = '' OR archive_items.metadata_key IS NULL OR archive_items.raw_metadata_key = '' OR archive_items.raw_metadata_key IS NULL").
		Order("archive_items.id DESC").Scan(&rows).Error
	if err != nil {
		return summary, err
	}
	summary.TargetItems = len(rows)
	groups := make(map[string]videoMetadataCandidate)
	for _, row := range rows {
		if _, exists := groups[row.Identity]; !exists {
			groups[row.Identity] = row
		}
	}
	candidates := make([]videoMetadataCandidate, 0, len(groups))
	for _, row := range groups {
		candidates = append(candidates, row)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ItemID > candidates[j].ItemID })
	if opts.Limit > 0 && len(candidates) > opts.Limit {
		candidates = candidates[:opts.Limit]
	}
	summary.TargetGroups = len(candidates)
	if opts.DryRun {
		return summary, nil
	}
	if client == nil {
		return summary, errors.New("River client is not configured")
	}
	for i, candidate := range candidates {
		args := VideoMetadataBackfillJobArgs{Identity: strings.TrimSpace(candidate.Identity), URL: candidate.Original, ShortID: candidate.ShortID, Version: 2}
		if _, err := client.Insert(ctx, args, &river.InsertOpts{Queue: videoMetadataBackfillQueue, MaxAttempts: 2,
			ScheduledAt: now.Add(time.Duration(i) * time.Second), Tags: []string{"video-metadata-backfill", "run-" + summary.RunID},
			UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 24 * time.Hour}}); err != nil {
			return summary, err
		}
		summary.Enqueued++
	}
	return summary, nil
}
