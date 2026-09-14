package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// TurnoverGranularity is the bucket width for AnalyticsTurnover.
//
// A closed type with unexported-elsewhere constants, the same reason
// treeTable exists in tree.go: the value is interpolated into the query's
// date_trunc call, so it must never be able to arrive from a caller-supplied
// string.
type TurnoverGranularity string

const (
	GranularityWeek  TurnoverGranularity = "week"
	GranularityMonth TurnoverGranularity = "month"
)

// LocationDistributionRow is one row of the dashboard's per-location bar
// chart: a location's live stock total, with its full ancestor path already
// resolved for display (docs/specs/11-reporting-and-analytics.md).
type LocationDistributionRow struct {
	LocationID   uuid.UUID
	LocationName string
	ItemCount    int
}

// TurnoverRow is one period of the dashboard's in/out trend chart: how much
// stock arrived (purchase + vision_ingestion) versus left (consumption) in
// that period.
type TurnoverRow struct {
	Period    string
	Purchased int
	Consumed  int
}

// AnalyticsTotalItems is the storage's total stock: SUM(inventory_batches.quantity)
// across every product in storageID, computed live at query time rather than
// from a stored counter — the same reason ReorderProducts never stores
// current_stock (docs/specs/10-reorder-and-shopping-export.md).
func (s *Store) AnalyticsTotalItems(ctx context.Context, storageID uuid.UUID) (int, error) {
	total, err := scanCount(ctx, s.pool, `
		SELECT coalesce(SUM(b.quantity), 0)
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		 WHERE p.storage_id = $1`, storageID)
	if err != nil {
		return 0, fmt.Errorf("store: analytics total items: %w", err)
	}
	return total, nil
}

// AnalyticsLocationDistribution sums live stock per location, grouped, with
// each location's full ancestor path resolved server-side (e.g. "Basement >
// Right Shelf") — the same recursive-ancestor-walk shape as categoryPathOf in
// ingestion.go, run once per location that actually holds stock rather than
// once per product.
//
// Only locations with at least one batch appear: an empty shelf contributes
// nothing to a chart of where stock sits.
func (s *Store) AnalyticsLocationDistribution(ctx context.Context, storageID uuid.UUID) ([]LocationDistributionRow, error) {
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE totals AS (
			SELECT b.location_id, SUM(b.quantity) AS item_count
			  FROM inventory_batches b
			  JOIN products p ON p.id = b.product_id
			 WHERE p.storage_id = $1
			 GROUP BY b.location_id
		),
		chain AS (
			SELECT l.id AS origin_id, l.id, l.parent_id, l.name, 0 AS depth
			  FROM locations l
			  JOIN totals t ON t.location_id = l.id
			UNION ALL
			SELECT chain.origin_id, l.id, l.parent_id, l.name, chain.depth + 1
			  FROM locations l
			  JOIN chain ON l.id = chain.parent_id
			 WHERE chain.depth < $2
		)
		SELECT c.origin_id, string_agg(c.name, ' > ' ORDER BY c.depth DESC) AS full_path, t.item_count
		  FROM chain c
		  JOIN totals t ON t.location_id = c.origin_id
		 GROUP BY c.origin_id, t.item_count
		 ORDER BY t.item_count DESC, full_path`, storageID, maxTreeDepth)
	if err != nil {
		return nil, fmt.Errorf("store: analytics location distribution: %w", err)
	}
	defer rows.Close()

	out := []LocationDistributionRow{}
	for rows.Next() {
		var r LocationDistributionRow
		if err := rows.Scan(&r.LocationID, &r.LocationName, &r.ItemCount); err != nil {
			return nil, fmt.Errorf("store: scan analytics location distribution: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AnalyticsTurnover aggregates inventory_logs by period, summing positive
// movement (purchase + vision_ingestion) and negative movement (consumption)
// separately so the frontend can render an in/out trend chart
// (docs/specs/11-reporting-and-analytics.md).
//
// consumption rows carry a negative change_qty (internal/store/consumption.go
// writes -dec.Quantity), so consumed is negated back to a positive count here
// rather than asking the frontend to know the sign convention of the ledger.
//
// This reconciles with the reorder dashboard by construction: both read
// inventory_batches/inventory_logs directly, and a product moving from
// in-stock to out-of-stock is exactly a consumption row landing in this same
// table, in this same period.
func (s *Store) AnalyticsTurnover(ctx context.Context, storageID uuid.UUID, granularity TurnoverGranularity) ([]TurnoverRow, error) {
	trunc := "month"
	format := "YYYY-MM"
	if granularity == GranularityWeek {
		trunc = "week"
		format = `IYYY-"W"IW`
	}

	// #nosec G201 -- trunc and format are chosen above from the closed
	// TurnoverGranularity type, never interpolated from caller input.
	sql := fmt.Sprintf(`
		SELECT to_char(date_trunc('%[1]s', l.timestamp), '%[2]s') AS period,
		       coalesce(SUM(CASE WHEN l.reason IN ('purchase', 'vision_ingestion') THEN l.change_qty ELSE 0 END), 0) AS purchased,
		       coalesce(SUM(CASE WHEN l.reason = 'consumption' THEN -l.change_qty ELSE 0 END), 0) AS consumed
		  FROM inventory_logs l
		  JOIN products p ON p.id = l.product_id
		 WHERE p.storage_id = $1
		 GROUP BY date_trunc('%[1]s', l.timestamp)
		 ORDER BY date_trunc('%[1]s', l.timestamp)`, trunc, format)

	rows, err := s.pool.Query(ctx, sql, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: analytics turnover: %w", err)
	}
	defer rows.Close()

	out := []TurnoverRow{}
	for rows.Next() {
		var r TurnoverRow
		if err := rows.Scan(&r.Period, &r.Purchased, &r.Consumed); err != nil {
			return nil, fmt.Errorf("store: scan analytics turnover: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
