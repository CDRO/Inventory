package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// fakeAnalyticsStore is an in-memory AnalyticsStore.
type fakeAnalyticsStore struct {
	totalItems      int
	totalErr        error
	distribution    []store.LocationDistributionRow
	distErr         error
	turnover        []store.TurnoverRow
	turnoverErr     error
	lastGranularity store.TurnoverGranularity
}

func (f *fakeAnalyticsStore) AnalyticsTotalItems(_ context.Context, _ uuid.UUID) (int, error) {
	return f.totalItems, f.totalErr
}

func (f *fakeAnalyticsStore) AnalyticsLocationDistribution(_ context.Context, _ uuid.UUID) ([]store.LocationDistributionRow, error) {
	return f.distribution, f.distErr
}

func (f *fakeAnalyticsStore) AnalyticsTurnover(_ context.Context, _ uuid.UUID, granularity store.TurnoverGranularity) ([]store.TurnoverRow, error) {
	f.lastGranularity = granularity
	return f.turnover, f.turnoverErr
}

// TestAnalyticsDashboardReturnsAllThreeMetrics — the acceptance criterion
// that all three metrics are present in one response, each shaped the way
// docs/specs/11-reporting-and-analytics.md's example JSON shows.
func TestAnalyticsDashboardReturnsAllThreeMetrics(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	locID := uuid.New()
	f.analytics.totalItems = 128
	f.analytics.distribution = []store.LocationDistributionRow{
		{LocationID: locID, LocationName: "Basement > Right Shelf", ItemCount: 37},
	}
	f.analytics.turnover = []store.TurnoverRow{
		{Period: "2026-08", Purchased: 42, Consumed: 35},
	}

	rec := f.do(http.MethodGet, f.base()+"/dashboard/analytics", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		TotalItems           int `json:"total_items"`
		LocationDistribution []struct {
			LocationID   string `json:"location_id"`
			LocationName string `json:"location_name"`
			ItemCount    int    `json:"item_count"`
		} `json:"location_distribution"`
		Turnover []struct {
			Period    string `json:"period"`
			Purchased int    `json:"purchased"`
			Consumed  int    `json:"consumed"`
		} `json:"turnover"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Equal(t, 128, body.TotalItems)
	require.Len(t, body.LocationDistribution, 1)
	assert.Equal(t, locID.String(), body.LocationDistribution[0].LocationID)
	assert.Equal(t, "Basement > Right Shelf", body.LocationDistribution[0].LocationName)
	assert.Equal(t, 37, body.LocationDistribution[0].ItemCount)
	require.Len(t, body.Turnover, 1)
	assert.Equal(t, "2026-08", body.Turnover[0].Period)
	assert.Equal(t, 42, body.Turnover[0].Purchased)
	assert.Equal(t, 35, body.Turnover[0].Consumed)
}

// TestAnalyticsDashboardDefaultsToMonthlyGranularity — no ?granularity means
// the store is asked for month, not left to its own default.
func TestAnalyticsDashboardDefaultsToMonthlyGranularity(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodGet, f.base()+"/dashboard/analytics", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, store.GranularityMonth, f.analytics.lastGranularity)
}

// TestAnalyticsDashboardAcceptsWeekGranularity — the ?granularity=week query
// param is threaded through to the store call unchanged.
func TestAnalyticsDashboardAcceptsWeekGranularity(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodGet, f.base()+"/dashboard/analytics?granularity=week", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, store.GranularityWeek, f.analytics.lastGranularity)
}

// TestAnalyticsDashboardRejectsInvalidGranularity — anything other than week
// or month is a validation failure, not silently treated as month.
func TestAnalyticsDashboardRejectsInvalidGranularity(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodGet, f.base()+"/dashboard/analytics?granularity=daily", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// TestAnalyticsDashboardRequiresSession — the analytics route sits behind the
// same gate chain as every other storage-scoped route.
func TestAnalyticsDashboardRequiresSession(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.anonymous(http.MethodGet, f.base()+"/dashboard/analytics", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
