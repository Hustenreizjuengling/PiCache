package logs

import (
	"context"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// migrations of component "logs". Append only: a released step is never
// edited (v1 keeps the column names of 0.1.x, later steps rename them). All
// timestamps and buckets are unix milliseconds; buckets are the start of the
// minute/hour.
var migrations = []string{
	// v1
	`CREATE TABLE logs_queries (
		id          INTEGER PRIMARY KEY,
		ts          INTEGER NOT NULL,
		client_ip   TEXT    NOT NULL,
		client_name TEXT    NOT NULL DEFAULT '',
		qname       TEXT    NOT NULL,
		qtype       TEXT    NOT NULL,
		status      TEXT    NOT NULL,
		rcode       TEXT    NOT NULL DEFAULT '',
		reason      TEXT    NOT NULL DEFAULT '',
		list_id     INTEGER NOT NULL DEFAULT 0,
		rule_id     INTEGER NOT NULL DEFAULT 0,
		service     TEXT    NOT NULL DEFAULT '',
		upstream    TEXT    NOT NULL DEFAULT '',
		duration_us INTEGER NOT NULL DEFAULT 0,
		answer      TEXT    NOT NULL DEFAULT '',
		dnssec      INTEGER NOT NULL DEFAULT 0,
		protocol    TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX logs_queries_ts ON logs_queries (ts);
	CREATE INDEX logs_queries_client ON logs_queries (client_ip, ts);
	CREATE INDEX logs_queries_qname ON logs_queries (qname, ts);

	CREATE TABLE logs_cache_requests (
		id           INTEGER PRIMARY KEY,
		ts           INTEGER NOT NULL,
		client_ip    TEXT    NOT NULL,
		client_name  TEXT    NOT NULL DEFAULT '',
		service      TEXT    NOT NULL,
		host         TEXT    NOT NULL,
		path         TEXT    NOT NULL,
		method       TEXT    NOT NULL,
		status       INTEGER NOT NULL,
		cache_status TEXT    NOT NULL,
		byte_range   TEXT    NOT NULL DEFAULT '',
		bytes_sent   INTEGER NOT NULL DEFAULT 0,
		bytes_hit    INTEGER NOT NULL DEFAULT 0,
		bytes_wan    INTEGER NOT NULL DEFAULT 0,
		bytes_stored INTEGER NOT NULL DEFAULT 0,
		duration_ms  INTEGER NOT NULL DEFAULT 0,
		group_key    TEXT    NOT NULL DEFAULT '',
		label        TEXT    NOT NULL DEFAULT '',
		user_agent   TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX logs_cache_requests_ts ON logs_cache_requests (ts);
	CREATE INDEX logs_cache_requests_client ON logs_cache_requests (client_ip, ts);

	CREATE TABLE logs_sni (
		id          INTEGER PRIMARY KEY,
		ts          INTEGER NOT NULL,
		client_ip   TEXT    NOT NULL,
		client_name TEXT    NOT NULL DEFAULT '',
		sni         TEXT    NOT NULL,
		service     TEXT    NOT NULL,
		bytes_up    INTEGER NOT NULL DEFAULT 0,
		bytes_down  INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX logs_sni_ts ON logs_sni (ts);

	CREATE TABLE logs_evictions (
		id        INTEGER PRIMARY KEY,
		ts        INTEGER NOT NULL,
		store_id  TEXT    NOT NULL,
		object_id TEXT    NOT NULL,
		service   TEXT    NOT NULL,
		group_key TEXT    NOT NULL DEFAULT '',
		bytes     INTEGER NOT NULL DEFAULT 0,
		reason    TEXT    NOT NULL
	);
	CREATE INDEX logs_evictions_ts ON logs_evictions (ts);

	CREATE TABLE logs_downloads (
		id          INTEGER PRIMARY KEY,
		client_ip   TEXT    NOT NULL,
		client_name TEXT    NOT NULL DEFAULT '',
		service     TEXT    NOT NULL,
		group_key   TEXT    NOT NULL,
		label       TEXT    NOT NULL DEFAULT '',
		first_seen  INTEGER NOT NULL,
		last_seen   INTEGER NOT NULL,
		requests    INTEGER NOT NULL DEFAULT 0,
		bytes_sent  INTEGER NOT NULL DEFAULT 0,
		bytes_hit   INTEGER NOT NULL DEFAULT 0,
		bytes_wan   INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX logs_downloads_last ON logs_downloads (last_seen);
	CREATE INDEX logs_downloads_group ON logs_downloads (service, group_key, client_ip);

	CREATE TABLE logs_dns_minute (
		bucket      INTEGER PRIMARY KEY,
		total       INTEGER NOT NULL DEFAULT 0,
		forwarded   INTEGER NOT NULL DEFAULT 0,
		cached      INTEGER NOT NULL DEFAULT 0,
		stale       INTEGER NOT NULL DEFAULT 0,
		local       INTEGER NOT NULL DEFAULT 0,
		special     INTEGER NOT NULL DEFAULT 0,
		lancache    INTEGER NOT NULL DEFAULT 0,
		blocked     INTEGER NOT NULL DEFAULT 0,
		other       INTEGER NOT NULL DEFAULT 0,
		duration_us INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE logs_dns_hourly (
		bucket      INTEGER PRIMARY KEY,
		total       INTEGER NOT NULL DEFAULT 0,
		forwarded   INTEGER NOT NULL DEFAULT 0,
		cached      INTEGER NOT NULL DEFAULT 0,
		stale       INTEGER NOT NULL DEFAULT 0,
		local       INTEGER NOT NULL DEFAULT 0,
		special     INTEGER NOT NULL DEFAULT 0,
		lancache    INTEGER NOT NULL DEFAULT 0,
		blocked     INTEGER NOT NULL DEFAULT 0,
		other       INTEGER NOT NULL DEFAULT 0,
		duration_us INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE logs_cache_minute (
		bucket        INTEGER NOT NULL,
		service       TEXT    NOT NULL,
		requests      INTEGER NOT NULL DEFAULT 0,
		bytes_sent    INTEGER NOT NULL DEFAULT 0,
		bytes_hit     INTEGER NOT NULL DEFAULT 0,
		bytes_wan     INTEGER NOT NULL DEFAULT 0,
		bytes_stored  INTEGER NOT NULL DEFAULT 0,
		sni_conns     INTEGER NOT NULL DEFAULT 0,
		sni_bytes     INTEGER NOT NULL DEFAULT 0,
		evictions     INTEGER NOT NULL DEFAULT 0,
		evicted_bytes INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (bucket, service)
	) WITHOUT ROWID;
	CREATE TABLE logs_cache_hourly (
		bucket        INTEGER NOT NULL,
		service       TEXT    NOT NULL,
		requests      INTEGER NOT NULL DEFAULT 0,
		bytes_sent    INTEGER NOT NULL DEFAULT 0,
		bytes_hit     INTEGER NOT NULL DEFAULT 0,
		bytes_wan     INTEGER NOT NULL DEFAULT 0,
		bytes_stored  INTEGER NOT NULL DEFAULT 0,
		sni_conns     INTEGER NOT NULL DEFAULT 0,
		sni_bytes     INTEGER NOT NULL DEFAULT 0,
		evictions     INTEGER NOT NULL DEFAULT 0,
		evicted_bytes INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (bucket, service)
	) WITHOUT ROWID;

	CREATE TABLE logs_dns_top_hourly (
		bucket      INTEGER NOT NULL,
		kind        TEXT    NOT NULL,
		key         TEXT    NOT NULL,
		label       TEXT    NOT NULL DEFAULT '',
		count       INTEGER NOT NULL DEFAULT 0,
		blocked     INTEGER NOT NULL DEFAULT 0,
		duration_us INTEGER NOT NULL DEFAULT 0,
		last_seen   INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (bucket, kind, key)
	) WITHOUT ROWID;
	CREATE TABLE logs_cache_top_hourly (
		bucket     INTEGER NOT NULL,
		kind       TEXT    NOT NULL,
		service    TEXT    NOT NULL DEFAULT '',
		key        TEXT    NOT NULL,
		label      TEXT    NOT NULL DEFAULT '',
		requests   INTEGER NOT NULL DEFAULT 0,
		bytes_sent INTEGER NOT NULL DEFAULT 0,
		bytes_hit  INTEGER NOT NULL DEFAULT 0,
		bytes_wan  INTEGER NOT NULL DEFAULT 0,
		last_seen  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (bucket, kind, service, key)
	) WITHOUT ROWID;`,
	// v2 (0.2.0): the status of the download cache DNS answers is "override"
	// (0.1.x wrote the old name): rename the rollup column and rewrite the
	// logged queries. The top lists and sessions store no status.
	`ALTER TABLE logs_dns_minute RENAME COLUMN lancache TO override;
	ALTER TABLE logs_dns_hourly RENAME COLUMN lancache TO override;
	UPDATE logs_queries SET status = 'override' WHERE status = 'lancache';`,
	// v3 (0.9.0): the upstream's Extended DNS Error (code -1 = none) and the
	// client subnet a client sent. Adding columns with constant defaults
	// does not rewrite the table.
	`ALTER TABLE logs_queries ADD COLUMN upstream_ede_code INTEGER NOT NULL DEFAULT -1;
	ALTER TABLE logs_queries ADD COLUMN upstream_ede_text TEXT NOT NULL DEFAULT '';
	ALTER TABLE logs_queries ADD COLUMN ecs TEXT NOT NULL DEFAULT '';`,
	// v4 (0.12.0): the upstream's answer when it differs from the final
	// one, the warning history (ack_ts 0 = not acknowledged) and the daily
	// top tables (bucket = start of the UTC day; same columns and keys as
	// the hourly ones). The top kinds qtype and unique need no step.
	`ALTER TABLE logs_queries ADD COLUMN upstream_answer TEXT NOT NULL DEFAULT '';

	CREATE TABLE logs_events (
		id       INTEGER PRIMARY KEY,
		ts       INTEGER NOT NULL,
		last_ts  INTEGER NOT NULL,
		count    INTEGER NOT NULL DEFAULT 1,
		event    TEXT    NOT NULL,
		severity TEXT    NOT NULL,
		title    TEXT    NOT NULL,
		message  TEXT    NOT NULL DEFAULT '',
		ack_ts   INTEGER NOT NULL DEFAULT 0,
		ack_by   TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX logs_events_last ON logs_events (last_ts);

	CREATE TABLE logs_dns_top_daily (
		bucket      INTEGER NOT NULL,
		kind        TEXT    NOT NULL,
		key         TEXT    NOT NULL,
		label       TEXT    NOT NULL DEFAULT '',
		count       INTEGER NOT NULL DEFAULT 0,
		blocked     INTEGER NOT NULL DEFAULT 0,
		duration_us INTEGER NOT NULL DEFAULT 0,
		last_seen   INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (bucket, kind, key)
	) WITHOUT ROWID;
	CREATE TABLE logs_cache_top_daily (
		bucket     INTEGER NOT NULL,
		kind       TEXT    NOT NULL,
		service    TEXT    NOT NULL DEFAULT '',
		key        TEXT    NOT NULL,
		label      TEXT    NOT NULL DEFAULT '',
		requests   INTEGER NOT NULL DEFAULT 0,
		bytes_sent INTEGER NOT NULL DEFAULT 0,
		bytes_hit  INTEGER NOT NULL DEFAULT 0,
		bytes_wan  INTEGER NOT NULL DEFAULT 0,
		last_seen  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (bucket, kind, service, key)
	) WITHOUT ROWID;`,
}

// enableAutoVacuum switches a brand-new logs.db to incremental auto-vacuum so
// that space freed by pruning can be returned to the filesystem. SQLite only
// allows this before the first table exists (VACUUM of an empty database is
// instant). It reports whether incremental auto-vacuum is active.
func enableAutoVacuum(ctx context.Context, d *db.DB) (bool, error) {
	var tables int
	if err := d.W.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema`).Scan(&tables); err != nil {
		return false, err
	}
	if tables == 0 {
		if _, err := d.W.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
			return false, err
		}
		if _, err := d.W.ExecContext(ctx, `VACUUM`); err != nil {
			return false, err
		}
	}
	var mode int
	if err := d.W.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return false, err
	}
	return mode == 2, nil
}
