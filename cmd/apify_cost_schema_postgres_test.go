package main

import (
	"fmt"
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestPostgresApifyCostMigrationOnLegacyTable(t *testing.T) {
	dsn := os.Getenv("ARKER_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ARKER_TEST_POSTGRES_DSN for the PostgreSQL migration test")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	adminSQL, err := admin.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer adminSQL.Close()
	name := fmt.Sprintf("arker_apify_test_%d", time.Now().UnixNano())
	if err := admin.Exec("CREATE DATABASE " + name).Error; err != nil {
		t.Fatal(err)
	}
	defer admin.Exec("DROP DATABASE " + name)
	db, err := gorm.Open(postgres.Open(withDatabase(dsn, name)), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := db.Exec("CREATE TABLE fallback_usages (id bigint primary key, cost_usd numeric); INSERT INTO fallback_usages VALUES (1, 0.00417)").Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := ensureApifyCostSchema(db); err != nil {
			t.Fatal(err)
		}
	}
	var row struct {
		CostUSD          float64
		CostReconciledAt *time.Time
	}
	if err := db.Raw("SELECT cost_usd, cost_reconciled_at FROM fallback_usages WHERE id=1").Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.CostUSD != .00417 || row.CostReconciledAt != nil {
		t.Fatalf("migration changed old billing evidence: %+v", row)
	}
}
