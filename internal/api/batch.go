package api

import (
	"net/http"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Bounds of the batch routes (POST …/batch).
const (
	maxBatchIDs      = 1000
	maxBatchAuditIDs = 100
)

// batchRequest is the body of the batch routes: delete, enable or disable
// the entities ids (all or nothing). Force is only for /filter/lists/batch
// (the entry budget).
type batchRequest struct {
	Action string  `json:"action"`
	IDs    []int64 `json:"ids"`
	Force  *bool   `json:"force"`
}

// batchResult is the answer of the batch routes: the entities whose state
// changed (deleting counts every id).
type batchResult struct {
	Changed int `json:"changed"`
}

// decodeBatch reads and validates a batch body: action delete, enable or
// disable (400 action); ids 1–1000 unique positive IDs (400 ids); force
// true only where allowed (400 force).
func decodeBatch(w http.ResponseWriter, r *http.Request, allowForce bool) (batchRequest, error) {
	var in batchRequest
	if err := decode(w, r, &in); err != nil {
		return in, err
	}
	in.Action = strings.ToLower(strings.TrimSpace(in.Action))
	switch in.Action {
	case "delete", "enable", "disable":
	default:
		return in, apperr.Invalid("action", "must be delete, enable or disable")
	}
	if len(in.IDs) == 0 || len(in.IDs) > maxBatchIDs {
		return in, apperr.Invalid("ids", "between 1 and %d ids are required", maxBatchIDs)
	}
	seen := make(map[int64]bool, len(in.IDs))
	for _, id := range in.IDs {
		if id <= 0 {
			return in, apperr.Invalid("ids", "invalid id %d", id)
		}
		if seen[id] {
			return in, apperr.Invalid("ids", "id %d is listed twice", id)
		}
		seen[id] = true
	}
	if !allowForce && in.Force != nil && *in.Force {
		return in, apperr.Invalid("force", "only for lists (the entry budget)")
	}
	return in, nil
}

// auditBatch records a batch change once: its action, count and the first
// 100 ids.
func (s *Server) auditBatch(r *http.Request, action string, in batchRequest) {
	ids := in.IDs
	if len(ids) > maxBatchAuditIDs {
		ids = ids[:maxBatchAuditIDs]
	}
	s.audit(r, action, "", map[string]any{"action": in.Action, "count": len(in.IDs), "ids": ids})
}
