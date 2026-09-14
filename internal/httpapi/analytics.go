package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// AnalyticsStore is the slice of the store the dashboard analytics handler
// uses.
type AnalyticsStore interface {
	AnalyticsTotalItems(ctx context.Context, storageID uuid.UUID) (int, error)
	AnalyticsLocationDistribution(ctx context.Context, storageID uuid.UUID) ([]store.LocationDistributionRow, error)
	AnalyticsTurnover(ctx context.Context, storageID uuid.UUID, granularity store.TurnoverGranularity) ([]store.TurnoverRow, error)
}

// AnalyticsHandler serves docs/specs/11-reporting-and-analytics.md.
type AnalyticsHandler struct {
	store  AnalyticsStore
	errors *ErrorWriter
}

// NewAnalyticsHandler wires the handler to its store.
func NewAnalyticsHandler(s AnalyticsStore, errs *ErrorWriter) *AnalyticsHandler {
	return &AnalyticsHandler{store: s, errors: errs}
}

type locationDistributionEntry struct {
	LocationID   uuid.UUID `json:"location_id"`
	LocationName string    `json:"location_name"`
	ItemCount    int       `json:"item_count"`
}

type turnoverEntry struct {
	Period    string `json:"period"`
	Purchased int    `json:"purchased"`
	Consumed  int    `json:"consumed"`
}

type analyticsResponse struct {
	TotalItems           int                         `json:"total_items"`
	LocationDistribution []locationDistributionEntry `json:"location_distribution"`
	Turnover             []turnoverEntry             `json:"turnover"`
}

// Dashboard serves GET /api/storages/{storage_id}/dashboard/analytics.
//
// Every metric is read live from inventory_batches/inventory_logs, scoped to
// storageID by the three store queries it calls — there is no separate
// analytics table for this handler to keep in sync, and no query here ever
// spans more than one storage (docs/specs/03-auth-and-multi-tenancy.md).
func (h *AnalyticsHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	granularity := store.GranularityMonth
	if raw := r.URL.Query().Get("granularity"); raw != "" {
		switch store.TurnoverGranularity(raw) {
		case store.GranularityWeek:
			granularity = store.GranularityWeek
		case store.GranularityMonth:
			granularity = store.GranularityMonth
		default:
			h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
				"granularity": {"Must be week or month."},
			}, nil))
			return
		}
	}

	totalItems, err := h.store.AnalyticsTotalItems(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "analytics total items"))
		return
	}

	distributionRows, err := h.store.AnalyticsLocationDistribution(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "analytics location distribution"))
		return
	}
	distribution := make([]locationDistributionEntry, 0, len(distributionRows))
	for _, row := range distributionRows {
		distribution = append(distribution, locationDistributionEntry{
			LocationID: row.LocationID, LocationName: row.LocationName, ItemCount: row.ItemCount,
		})
	}

	turnoverRows, err := h.store.AnalyticsTurnover(r.Context(), storageID, granularity)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "analytics turnover"))
		return
	}
	turnover := make([]turnoverEntry, 0, len(turnoverRows))
	for _, row := range turnoverRows {
		turnover = append(turnover, turnoverEntry{Period: row.Period, Purchased: row.Purchased, Consumed: row.Consumed})
	}

	writeJSON(w, http.StatusOK, analyticsResponse{
		TotalItems: totalItems, LocationDistribution: distribution, Turnover: turnover,
	})
}
