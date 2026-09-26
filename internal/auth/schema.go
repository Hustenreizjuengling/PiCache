package auth

import "slices"

// migrations are the append-only schema steps of component "auth".
var migrations = []string{
	`CREATE TABLE auth_users (
		id              INTEGER PRIMARY KEY,
		username        TEXT    NOT NULL UNIQUE COLLATE NOCASE,
		password_hash   TEXT    NOT NULL,
		totp_secret     TEXT,
		totp_pending    TEXT,
		totp_pending_at INTEGER NOT NULL DEFAULT 0,
		totp_last_step  INTEGER NOT NULL DEFAULT 0,
		created_at      INTEGER NOT NULL,
		last_login_at   INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE auth_sessions (
		id         TEXT    PRIMARY KEY,
		hash       BLOB    NOT NULL UNIQUE,
		user_id    INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
		created_at INTEGER NOT NULL,
		last_seen  INTEGER NOT NULL,
		ip         TEXT    NOT NULL,
		user_agent TEXT    NOT NULL
	);
	CREATE INDEX auth_sessions_user ON auth_sessions(user_id);
	CREATE TABLE auth_tokens (
		id         INTEGER PRIMARY KEY,
		hash       BLOB    NOT NULL UNIQUE,
		user_id    INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
		name       TEXT    NOT NULL,
		scope      TEXT    NOT NULL CHECK (scope IN ('read', 'admin')),
		prefix     TEXT    NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL DEFAULT 0,
		last_used  INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE auth_audit (
		id       INTEGER PRIMARY KEY,
		time     INTEGER NOT NULL,
		username TEXT    NOT NULL,
		ip       TEXT    NOT NULL,
		action   TEXT    NOT NULL,
		target   TEXT    NOT NULL,
		details  TEXT    NOT NULL DEFAULT '',
		count    INTEGER NOT NULL DEFAULT 1
	);
	CREATE INDEX auth_audit_time ON auth_audit(time);`,
	// v2 (0.11.0): roles. Existing accounts become admins; an insert that
	// forgets the role creates a viewer (fail closed).
	`ALTER TABLE auth_users ADD COLUMN role TEXT NOT NULL DEFAULT 'viewer'
		CHECK (role IN ('admin', 'viewer'));
	UPDATE auth_users SET role = 'admin';`,
}

// Migrations returns the schema steps of component "auth" in picache.db
// (`picache db salvage` builds a fresh schema with them).
func Migrations() []string { return slices.Clone(migrations) }
