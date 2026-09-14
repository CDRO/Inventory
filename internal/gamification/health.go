package gamification

// HealthScore is a storage's inventory health, the mean of five sub-scores
// (docs/specs/51-gamification-scoring.md). It is always a storage-level
// metric, never a per-user one: nobody is blamed for the household's
// backlog.
//
// Each argument is a percentage (0-100) of the storage's products or
// batches that satisfy one criterion:
//
//   - categorized: has a category_id set.
//   - imaged: has an image or icon.
//   - minStockTracked: has min_stock > 0.
//   - expiryTracked: perishable/long-shelf-life batches carrying an
//     expiration_date.
//   - recentlyActive: touched by an inventory_logs row in the last 180 days.
//
// A storage with no products to measure a sub-score against (0/0) is the
// caller's problem, not this function's: pass 0 for that sub-score rather
// than NaN, so an empty storage reads as "nothing scored yet" instead of
// producing an unusable result.
func HealthScore(categorized, imaged, minStockTracked, expiryTracked, recentlyActive float64) float64 {
	return (categorized + imaged + minStockTracked + expiryTracked + recentlyActive) / 5
}
