package parental

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// The follower sync (docs/ARCHITECTURE.md 15.4): the parental configuration
// of the groups (blocked services, schedules, safe search) replaced by the
// primary's; the overrides and pauses of this follower are kept for the
// groups whose name stays (they are runtime state, never synced).

// SyncGroup is the parental configuration of one group of a primary (the
// group id is already this follower's).
type SyncGroup struct {
	GroupID int64
	Config  Config
}

// SyncKept is a follower's override and pause of a group.
type SyncKept struct {
	OverrideMode  string
	OverrideUntil int64 // unix ms, 0 = none
	PauseUntil    int64 // unix ms, 0 = none
}

// ValidateSync checks and normalises the configurations of a sync like a
// PUT /parental/groups/{id} (services, schedules, safe search); the field
// names the group.
func ValidateSync(groups []SyncGroup) ([]SyncGroup, error) {
	out := make([]SyncGroup, 0, len(groups))
	seen := map[int64]bool{}
	for _, g := range groups {
		if seen[g.GroupID] {
			return nil, apperr.Invalid("parental", "group %d is listed twice", g.GroupID)
		}
		seen[g.GroupID] = true
		cfg, err := validateConfig(g.Config)
		if err != nil {
			return nil, fmt.Errorf("parental controls of group %d: %w", g.GroupID, err)
		}
		if !validYouTube(g.Config.SafeSearch.YouTube) && g.Config.SafeSearch.YouTube != "" {
			return nil, apperr.Invalid("safeSearch.youtube", `must be "off", "moderate" or "strict"`)
		}
		cfg.SafeSearch = g.Config.SafeSearch
		if cfg.SafeSearch.YouTube == "" {
			cfg.SafeSearch.YouTube = YouTubeOff
		}
		out = append(out, SyncGroup{GroupID: g.GroupID, Config: cfg})
	}
	return out, nil
}

// KeptByGroupName reads the overrides and pauses of this follower by group
// name (lower-case) before a sync replaces the groups (tx).
func KeptByGroupName(ctx context.Context, tx *sql.Tx) (map[string]SyncKept, error) {
	rows, err := tx.QueryContext(ctx, `SELECT g.name, p.override_mode, COALESCE(p.override_until, 0), COALESCE(p.pause_until, 0)
		FROM parental_groups p JOIN client_groups g ON g.id = p.group_id
		WHERE p.override_mode != '' OR p.pause_until IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SyncKept{}
	for rows.Next() {
		var name string
		var k SyncKept
		if err := rows.Scan(&name, &k.OverrideMode, &k.OverrideUntil, &k.PauseUntil); err != nil {
			return nil, err
		}
		out[strings.ToLower(name)] = k
	}
	return out, rows.Err()
}

// ReplaceSynced replaces the parental configuration of every group in tx
// with groups (validated by ValidateSync); keep holds the overrides and
// pauses to keep (by this follower's group id).
func ReplaceSynced(ctx context.Context, tx *sql.Tx, groups []SyncGroup, keep map[int64]SyncKept) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM parental_groups`); err != nil {
		return err
	}
	now := db.NowMs()
	written := map[int64]bool{}
	for _, g := range groups {
		b, err := json.Marshal(g.Config)
		if err != nil {
			return err
		}
		k := keep[g.GroupID]
		if _, err := tx.ExecContext(ctx, `INSERT INTO parental_groups (group_id, config, override_mode, override_until, pause_until, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, g.GroupID, string(b), k.OverrideMode, nullMs(k.OverrideUntil), nullMs(k.PauseUntil), now); err != nil {
			return fmt.Errorf("parental controls of group %d: %w", g.GroupID, err)
		}
		written[g.GroupID] = true
	}
	for id, k := range keep {
		if written[id] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO parental_groups (group_id, config, override_mode, override_until, pause_until, updated_at)
			VALUES (?, '{}', ?, ?, ?, ?)`, id, k.OverrideMode, nullMs(k.OverrideUntil), nullMs(k.PauseUntil), now); err != nil {
			return err
		}
	}
	return nil
}

func nullMs(ms int64) any {
	if ms == 0 {
		return nil
	}
	return ms
}
