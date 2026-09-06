package apify

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"arker/internal/models"
)

func TestSettlementIncludesPartialAndFailedRuns(t *testing.T) {
	for _, status := range []string{"SUCCEEDED", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			scripted := &actorRun{Actor: ActorPinterest, Status: status, CostUSD: .002, SettledCostUSD: .00417}
			client, db := newTestClient(t, newFakeNetwork(*scripted))
			client.costSettleDelays = []time.Duration{time.Millisecond, 2 * time.Millisecond}
			usage := &models.FallbackUsage{ArchiveItemID: 7}
			_, err := client.runActor(context.Background(), db, usage, ActorPinterest, map[string]any{}, io.Discard)
			if (err != nil) != (status == "FAILED") {
				t.Fatalf("run error = %v", err)
			}
			client.Close()
			// Finalizing a caller's stale, nonzero struct must not undo settlement.
			usage.Detail = "final detail"
			client.recordUsage(db, usage)
			rows := usageRows(t, db)
			if len(rows) != 1 || rows[0].CostUSD != .00417 || rows[0].CostReconciledAt == nil || rows[0].Detail != "final detail" {
				t.Fatalf("rows = %+v", rows)
			}
		})
	}
}

func TestReconciliationRecoversInterruptedSettlementAndIsIdempotent(t *testing.T) {
	network := newFakeNetwork(actorRun{Actor: ActorPinterest, CostUSD: .027})
	client, db := newTestClient(t, network)
	usage := &models.FallbackUsage{ArchiveItemID: 7}
	if _, err := client.runActor(context.Background(), db, usage, ActorPinterest, map[string]any{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	// Simulate an old process which saved a partial cost and then exited.
	db.Model(usage).Updates(map[string]any{"cost_usd": .002, "created_at": time.Now().Add(-time.Hour)})
	cursor, err := client.ReconcileCosts(context.Background(), db, 0, 100)
	if err != nil || cursor != usage.ID {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	rows := usageRows(t, db)
	if rows[0].CostUSD != .027 || rows[0].CostReconciledAt == nil {
		t.Fatalf("rows=%+v", rows)
	}
	cursor, err = client.ReconcileCosts(context.Background(), db, 0, 100)
	if err != nil || cursor != 0 {
		t.Fatalf("already reconciled cursor=%d err=%v", cursor, err)
	}
	if len(network.started) != 1 {
		t.Fatal("reconciliation started a new actor")
	}
}

type billingTransport func(*http.Request) (*http.Response, error)

func (f billingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReconciliationRejectsMissingBillingAndAcceptsZeroCorrections(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		wantErr    bool
		cost       float64
	}{
		{"missing", `{"id":"run-id","status":"SUCCEEDED"}`, true, .2},
		{"negative", `{"id":"run-id","status":"SUCCEEDED","usageTotalUsd":-1}`, true, .2},
		{"active", `{"id":"run-id","status":"RUNNING","usageTotalUsd":0}`, true, .2},
		{"wrong id", `{"id":"another","status":"FAILED","usageTotalUsd":0}`, true, .2},
		{"zero correction", `{"id":"run-id","status":"FAILED","usageTotalUsd":0}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, db := newTestClient(t, newFakeNetwork())
			row := models.FallbackUsage{Provider: models.FallbackProviderApify, OperationID: "run-id", CostUSD: .2}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			client.http.Transport = billingTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Fatal("billing must only GET")
				}
				return httpResponse(r, 200, []byte(`{"data":`+tc.data+`}`)), nil
			})
			_, err := client.reconcileRunCost(context.Background(), db, row.ID, row.OperationID, "test")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			rows := usageRows(t, db)
			if rows[0].CostUSD != tc.cost {
				t.Fatalf("cost=%f", rows[0].CostUSD)
			}
		})
	}
}

func TestSettlementRetriesTransientReadFailure(t *testing.T) {
	client, db := newTestClient(t, newFakeNetwork())
	row := models.FallbackUsage{Provider: models.FallbackProviderApify, OperationID: "run-id"}
	db.Create(&row)
	calls := 0
	client.http.Transport = billingTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return httpResponse(r, 503, []byte("temporary")), nil
		}
		return httpResponse(r, 200, []byte(`{"data":{"id":"run-id","status":"FAILED","usageTotalUsd":0.5}}`)), nil
	})
	client.costSettleDelays = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	client.settleCost(db, row.ID, row.OperationID, "test")
	client.Close()
	if rows := usageRows(t, db); rows[0].CostUSD != .5 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestReconciliationAdvancesPastUnavailableRun(t *testing.T) {
	client, db := newTestClient(t, newFakeNetwork())
	for _, id := range []string{"missing", "available"} {
		row := models.FallbackUsage{Provider: models.FallbackProviderApify, OperationID: id, CostUSD: .1}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		db.Model(&row).Update("created_at", time.Now().Add(-time.Hour))
	}
	client.http.Transport = billingTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v2/actor-runs/missing" {
			return httpResponse(r, 404, []byte("missing")), nil
		}
		return httpResponse(r, 200, []byte(`{"data":{"id":"available","status":"SUCCEEDED","usageTotalUsd":0.2}}`)), nil
	})
	cursor, err := client.ReconcileCosts(context.Background(), db, 0, 100)
	if err == nil || cursor != 2 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	rows := usageRows(t, db)
	if rows[0].CostUSD != .1 || rows[0].CostReconciledAt != nil || rows[1].CostUSD != .2 {
		t.Fatalf("rows=%+v", rows)
	}
	cursor, err = client.ReconcileCosts(context.Background(), db, cursor, 100)
	if err != nil || cursor != 0 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
}
