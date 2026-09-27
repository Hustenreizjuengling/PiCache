package clients

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// The follower sync (docs/ARCHITECTURE.md 15.4): the groups and clients of a
// primary replace this follower's, IDs included, in the app's transaction
// (the group-linked sections are replaced in the same transaction, so the
// IDs stay consistent). Everything is validated first with the rules and
// limits of the API.

// SyncedClients are the validated groups and clients of a sync.
type SyncedClients struct {
	groups  []syncedGroup
	clients []syncedClient
}

type syncedGroup struct {
	g   Group
	ups groupUpstreams
}

type syncedClient struct {
	c   Client
	in  ClientInput
	ids []identifier
}

// ValidateSync checks the groups and clients of a primary: the group
// rules (names unique, the Default group 1 present and without
// upstreams, the upstream syntax), the client rules (identifier syntax,
// every identifier with one client, groups that exist), the limits.
func ValidateSync(groups []Group, clients []Client) (*SyncedClients, error) {
	if len(groups) > maxGroups {
		return nil, apperr.Conflict("at most %d groups are allowed", maxGroups)
	}
	if len(clients) > maxClients {
		return nil, apperr.Conflict("at most %d clients are allowed", maxClients)
	}
	out := &SyncedClients{}
	ids, names := map[int64]bool{}, map[string]bool{}
	clientIDs := map[int64]bool{}
	for _, c := range clients {
		clientIDs[c.ID] = true
	}
	for _, g := range groups {
		in, err := validateGroup(GroupInput{Name: g.Name, Comment: g.Comment, Enabled: g.Enabled})
		if err != nil {
			return nil, fmt.Errorf("group %d: %w", g.ID, err)
		}
		if g.ID <= 0 || ids[g.ID] || names[strings.ToLower(in.Name)] {
			return nil, apperr.Conflict("group %q is listed twice", in.Name)
		}
		ids[g.ID], names[strings.ToLower(in.Name)] = true, true
		preset := g.UpstreamPreset
		ups, err := resolveGroupUpstreams(g.ID, nonNilStrings(g.Upstreams), &preset, groupUpstreams{Upstreams: []string{}})
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", in.Name, err)
		}
		if g.DeviceClientID != nil && !clientIDs[*g.DeviceClientID] {
			g.DeviceClientID = nil
		}
		g.Name, g.Comment = in.Name, in.Comment
		out.groups = append(out.groups, syncedGroup{g: g, ups: ups})
	}
	if !ids[DefaultGroupID] {
		return nil, apperr.Invalid("groups", "the Default group is missing")
	}
	owner := map[string]string{}
	seenClients := map[int64]bool{}
	for _, c := range clients {
		stats := c.IgnoreStats
		in, parsed, err := validateClient(ClientInput{Name: c.Name, Identifiers: c.Identifiers, GroupIDs: c.GroupIDs, Comment: c.Comment,
			DownloadCacheBypass: c.DownloadCacheBypass, IgnoreLogs: c.IgnoreLogs, IgnoreStats: &stats})
		if err != nil {
			return nil, fmt.Errorf("client %q: %w", c.Name, err)
		}
		if c.ID <= 0 || seenClients[c.ID] {
			return nil, apperr.Conflict("client %q is listed twice", c.Name)
		}
		seenClients[c.ID] = true
		for _, id := range parsed {
			if other, ok := owner[id.value]; ok {
				return nil, apperr.Conflict("%s belongs to client %s", id.value, other)
			}
			owner[id.value] = in.Name
		}
		for _, g := range in.GroupIDs {
			if !ids[g] {
				return nil, apperr.Invalid("groupIds", "client %q: group %d does not exist", in.Name, g)
			}
		}
		out.clients = append(out.clients, syncedClient{c: c, in: in, ids: parsed})
	}
	return out, nil
}

// GroupIDs returns the ids of the synced groups.
func (s *SyncedClients) GroupIDs() []int64 {
	out := make([]int64, 0, len(s.groups))
	for _, g := range s.groups {
		out = append(out, g.g.ID)
	}
	return out
}

// ReplaceSynced replaces every group, client, identifier and membership in
// tx with the synced ones (IDs of the primary). The links of the other
// sections to deleted groups cascade: the app replaces them in the same
// transaction.
func ReplaceSynced(ctx context.Context, tx *sql.Tx, s *SyncedClients) error {
	for _, q := range []string{`DELETE FROM client_groups`, `DELETE FROM client_clients`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	now := db.NowMs()
	for _, sc := range s.clients {
		c, in := sc.c, sc.in
		created, updated := db.Ms(c.CreatedAt), db.Ms(c.UpdatedAt)
		if created == 0 {
			created = now
		}
		if updated == 0 {
			updated = now
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO client_clients (id, name, comment, download_cache_bypass, ignore_logs, ignore_stats,
				created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, in.Name, in.Comment, in.DownloadCacheBypass, in.IgnoreLogs, in.ignoreStats(), created, updated); err != nil {
			return fmt.Errorf("client %q: %w", in.Name, err)
		}
		for i, id := range sc.ids {
			if _, err := tx.ExecContext(ctx, `INSERT INTO client_identifiers (value, client_id, kind, pos) VALUES (?, ?, ?, ?)`,
				id.value, c.ID, id.kind, i); err != nil {
				return fmt.Errorf("client %q: %w", in.Name, err)
			}
		}
	}
	for _, sg := range s.groups {
		g := sg.g
		created := db.Ms(g.CreatedAt)
		if created == 0 {
			created = now
		}
		var device any
		if g.DeviceClientID != nil {
			device = *g.DeviceClientID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO client_groups (id, name, comment, enabled, created_at, upstreams, upstream_preset,
				device_client_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			g.ID, g.Name, g.Comment, g.Enabled, created, encodeUpstreams(sg.ups.Upstreams), sg.ups.Preset, device); err != nil {
			return fmt.Errorf("group %q: %w", g.Name, err)
		}
	}
	for _, sc := range s.clients {
		for _, g := range sc.in.GroupIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO client_memberships (client_id, group_id) VALUES (?, ?)`, sc.c.ID, g); err != nil {
				return err
			}
		}
	}
	return checkUpstreamBounds(ctx, tx)
}

// GroupNames returns the groups of this follower by lower-case name (the
// name mapping of a group-linked section synced without its groups; q is
// the sync's transaction).
func GroupNames(ctx context.Context, q queryer) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name FROM client_groups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[strings.ToLower(name)] = id
	}
	return out, rows.Err()
}

// Reload rebuilds the snapshot from the database and notifies the
// listeners (after a sync replaced the tables in the app's transaction).
func (r *Registry) Reload(ctx context.Context) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if err := r.reload(ctx); err != nil {
		return err
	}
	r.changed()
	return nil
}

// nonNilStrings returns s or an empty list.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clone(s)
}
