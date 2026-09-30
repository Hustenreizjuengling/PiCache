// Package listing provides the generic page type used by list endpoints.
package listing

// Page is one page of a listing. Next is an opaque cursor for cursor-based
// listings (empty when there are no more items); Total is the total number of
// matching items for offset-based listings (-1 if unknown).
type Page[T any] struct {
	Items []T    `json:"items"`
	Total int    `json:"total"`
	Next  string `json:"next,omitempty"`
	// Partial: the listing stopped early (a query-log search ran out of its
	// time budget): Items may be fewer than asked for, and Next continues
	// where the search stopped.
	Partial bool `json:"partial,omitempty"`
}

// Clamp limits a requested page size to [1, max], using def for <= 0.
func Clamp(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}
