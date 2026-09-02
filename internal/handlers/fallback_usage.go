package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/models"
)

// usageTotals is one aggregate row of paid fallback spend.
type usageTotals struct {
	Provider  string  `json:"provider,omitempty"`
	Product   string  `json:"product,omitempty"`
	Events    int64   `json:"events"`
	Successes int64   `json:"successes"`
	Records   int64   `json:"records"`
	Bytes     int64   `json:"bytes_transferred"`
	CostUSD   float64 `json:"cost_usd"`
}

// usageDay is one day of spend for the recent-history series.
type usageDay struct {
	Day     string  `json:"day"`
	Events  int64   `json:"events"`
	CostUSD float64 `json:"cost_usd"`
}

// usageEntry is one recent usage row, trimmed for the API.
type usageEntry struct {
	CreatedAt   time.Time `json:"created_at"`
	ShortID     string    `json:"short_id"`
	URL         string    `json:"url"`
	Provider    string    `json:"provider"`
	Product     string    `json:"product"`
	OperationID string    `json:"operation_id,omitempty"`
	Records     int       `json:"records,omitempty"`
	Bytes       int64     `json:"bytes_transferred,omitempty"`
	CostUSD     float64   `json:"cost_usd"`
	Success     bool      `json:"success"`
	Detail      string    `json:"detail,omitempty"`
}

// FallbackUsage reports what the paid fallback has been spending: overall
// totals, per-provider/product totals, a per-day series for the last 30 days,
// and the most recent events. Apify rows carry the platform-reported run cost;
// historical Bright Data rows carry the rate-based estimate computed at the
// time. The provider's own billing dashboard remains the invoice of record.
func FallbackUsage(c *gin.Context, db *gorm.DB) {
	var overall usageTotals
	if err := db.Model(&models.FallbackUsage{}).
		Select("COUNT(*) AS events",
			"COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END), 0) AS successes",
			"COALESCE(SUM(records), 0) AS records",
			"COALESCE(SUM(bytes_transferred), 0) AS bytes",
			"COALESCE(SUM(cost_usd), 0) AS cost_usd").
		Scan(&overall).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	var byProduct []usageTotals
	if err := db.Model(&models.FallbackUsage{}).
		Select("provider", "product",
			"COUNT(*) AS events",
			"COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END), 0) AS successes",
			"COALESCE(SUM(records), 0) AS records",
			"COALESCE(SUM(bytes_transferred), 0) AS bytes",
			"COALESCE(SUM(cost_usd), 0) AS cost_usd").
		Group("provider").Group("product").Order("provider").Order("product").
		Scan(&byProduct).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	var days []usageDay
	if err := db.Model(&models.FallbackUsage{}).
		Select("TO_CHAR(created_at::date, 'YYYY-MM-DD') AS day",
			"COUNT(*) AS events",
			"COALESCE(SUM(cost_usd), 0) AS cost_usd").
		Where("created_at > ?", time.Now().AddDate(0, 0, -30)).
		Group("created_at::date").Order("day DESC").
		Scan(&days).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	var rows []models.FallbackUsage
	if err := db.Order("id DESC").Limit(50).Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}
	recent := make([]usageEntry, len(rows))
	for i, row := range rows {
		recent[i] = usageEntry{
			CreatedAt:   row.CreatedAt,
			ShortID:     row.ShortID,
			URL:         row.URL,
			Provider:    row.Provider,
			Product:     row.Product,
			OperationID: row.OperationID,
			Records:     row.Records,
			Bytes:       row.BytesTransferred,
			CostUSD:     row.CostUSD,
			Success:     row.Success,
			Detail:      row.Detail,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"note":         "Apify costs are the platform-reported run cost; historical Bright Data rows are rate-based estimates. The provider's billing dashboard is the invoice of record.",
		"total":        overall,
		"by_product":   byProduct,
		"last_30_days": days,
		"recent":       recent,
	})
}
