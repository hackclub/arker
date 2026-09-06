package apify

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"time"

	"arker/internal/models"
	"gorm.io/gorm"
)

// reconcileRunCost reads billing only: it never starts/resurrects an actor or
// reads its dataset/media. Missing billing permission is not a free run.
func (c *Client) reconcileRunCost(ctx context.Context, db *gorm.DB, usageID uint, runID, product string) (float64, error) {
	var response struct {
		Data struct {
			ID     string   `json:"id"`
			Status string   `json:"status"`
			Cost   *float64 `json:"usageTotalUsd"`
		} `json:"data"`
	}
	err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/actor-runs/%s", apiBase, url.PathEscape(runID)), nil, &response)
	if err != nil {
		return 0, err
	}
	r := response.Data
	if r.ID != runID || r.Cost == nil || math.IsNaN(*r.Cost) || math.IsInf(*r.Cost, 0) || *r.Cost < 0 {
		return 0, fmt.Errorf("run %s returned missing or invalid billing data", runID)
	}
	switch r.Status {
	case "SUCCEEDED", "FAILED", "ABORTED", "TIMED-OUT":
	default:
		return 0, fmt.Errorf("run %s is not terminal (%s)", runID, r.Status)
	}
	result := db.WithContext(ctx).Model(&models.FallbackUsage{}).
		Where("id = ? AND provider = ? AND operation_id = ?", usageID, models.FallbackProviderApify, runID).
		Updates(map[string]any{"cost_usd": *r.Cost, "cost_reconciled_at": time.Now().UTC()})
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected != 1 {
		return 0, fmt.Errorf("run %s has no matching usage row", runID)
	}
	c.checkRunCost(runID, product, *r.Cost)
	return *r.Cost, nil
}

// ReconcileCosts scans a bounded page in ID order. The cursor advances past
// failures so one inaccessible/deleted run cannot starve the rest. Reset it to
// zero after an empty page. Successful rows are rechecked daily; unknown rows
// are retried on the next sweep. Recent runs use the faster settlement path.
func (c *Client) ReconcileCosts(ctx context.Context, db *gorm.DB, afterID uint, limit int) (uint, error) {
	if limit < 1 || limit > 100 {
		return afterID, fmt.Errorf("cost reconciliation limit must be 1..100")
	}
	var rows []models.FallbackUsage
	now := time.Now().UTC()
	if err := db.WithContext(ctx).Where("provider = ? AND operation_id <> '' AND id > ? AND created_at < ?", models.FallbackProviderApify, afterID, now.Add(-5*time.Minute)).
		Where("cost_reconciled_at IS NULL OR cost_reconciled_at < ?", now.Add(-24*time.Hour)).Order("id").Limit(limit).Find(&rows).Error; err != nil {
		return afterID, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	var firstErr error
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return afterID, err
		}
		readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := c.reconcileRunCost(readCtx, db, row.ID, row.OperationID, row.Product)
		cancel()
		if err != nil {
			slog.Warn("Apify billing reconciliation failed", "run", row.OperationID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
		afterID = row.ID
	}
	return afterID, firstErr
}

// RunCostReconciler recovers settlement interrupted by redeploys and catches
// charges arriving after the fast three-minute window. Work resumes from the
// persisted reconciliation timestamps, not process-local goroutines.
func (c *Client) RunCostReconciler(ctx context.Context, db *gorm.DB) {
	var cursor uint
	for {
		next, err := c.ReconcileCosts(ctx, db, cursor, 100)
		cursor = next
		if err != nil {
			slog.Warn("Apify cost reconciliation batch incomplete", "error", err)
		}
		delay := time.Minute
		if cursor == 0 {
			delay = time.Hour
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
