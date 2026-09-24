package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/listing"
)

const (
	auditRetention    = 365 * 24 * time.Hour
	auditMaxRows      = 100_000
	auditMaxDetails   = 4096
	auditMaxField     = 256 // action, target, username, ip
	auditFailedWindow = 10 * time.Minute
	auditMaxSearch    = 100
	auditMaxOffset    = auditMaxRows

	actionLoginFailed = "auth.login_failed"
)

// redactedNames are the normalised (lower-case, without '_' and '-') JSON
// member names whose values never reach the audit log.
var redactedNames = map[string]bool{
	"password": true, "currentpassword": true, "newpassword": true, "setuptoken": true,
	"token": true, "secret": true, "code": true, "totp": true,
	"passwordsealed": true, "passphrase": true, "apikey": true, "privatekey": true,
	"authorization": true, "cookie": true, "credentials": true,
}

const redacted = "[redacted]"

// Audit records an action with redacted details (errors are logged).
func (a *Service) Audit(ctx context.Context, p *Principal, ip, action, target string, details any) {
	username := ""
	if p != nil {
		username = p.Username
		if p.TokenID != 0 {
			username += " (API token #" + strconv.FormatInt(p.TokenID, 10) + ")"
		}
	}
	_, err := a.db.W.ExecContext(context.WithoutCancel(ctx),
		`INSERT INTO auth_audit (time, username, ip, action, target, details) VALUES (?, ?, ?, ?, ?, ?)`,
		a.now().UnixMilli(), truncateUTF8(username, auditMaxField), truncateUTF8(ip, auditMaxField),
		truncateUTF8(action, auditMaxField), truncateUTF8(target, auditMaxField), auditDetails(details))
	if err != nil {
		a.log.Error("write audit log", slog.String("action", action), slog.Any("err", err))
	}
}

// auditFailure records a failed login or setup attempt. Failures from one
// client key within auditFailedWindow are aggregated into one row so that a
// guessing attack cannot flood the audit log.
func (a *Service) auditFailure(ctx context.Context, meta ReqMeta, reason string) {
	ctx = context.WithoutCancel(ctx)
	target := strings.TrimPrefix(clientThrottleKey(meta.IP), "c:")
	now := a.now()
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		var id, count int64
		err := tx.QueryRowContext(ctx, `SELECT id, count FROM auth_audit
			WHERE time >= ? AND action = ? AND target = ? ORDER BY id DESC LIMIT 1`,
			now.Add(-auditFailedWindow).UnixMilli(), actionLoginFailed, target).Scan(&id, &count)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `INSERT INTO auth_audit (time, username, ip, action, target, details)
				VALUES (?, '', ?, ?, ?, ?)`, now.UnixMilli(), truncateUTF8(meta.IP, auditMaxField), actionLoginFailed, target,
				auditDetails(map[string]any{"attempts": 1, "lastReason": reason}))
			return err
		case err != nil:
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE auth_audit SET count = ?, ip = ?, details = ? WHERE id = ?`,
			count+1, truncateUTF8(meta.IP, auditMaxField), auditDetails(map[string]any{
				"attempts": count + 1, "lastReason": reason, "lastAt": now.UTC().Format(time.RFC3339),
			}), id)
		return err
	})
	if err != nil {
		a.log.Error("write audit log", slog.String("action", actionLoginFailed), slog.Any("err", err))
	}
}

// auditDetails marshals details, redacts secrets by member name and
// truncates the result to auditMaxDetails bytes.
func auditDetails(details any) string {
	if details == nil {
		return ""
	}
	raw, err := json.Marshal(details, json.Deterministic(true))
	if err == nil {
		raw, err = redactJSON(raw)
	}
	if err != nil {
		return `{"error":"details could not be recorded"}`
	}
	if len(raw) > auditMaxDetails {
		return truncateUTF8(string(raw), auditMaxDetails-len("…")) + "…"
	}
	return string(raw)
}

// redactJSON replaces the value of every object member with a sensitive
// name (at any depth) by "[redacted]".
func redactJSON(in []byte) ([]byte, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(in))
	var out bytes.Buffer
	enc := jsontext.NewEncoder(&out)
	for {
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := enc.WriteToken(tok); err != nil {
			return nil, err
		}
		kind, length := dec.StackIndex(dec.StackDepth())
		isName := tok.Kind() == '"' && kind == '{' && length%2 == 1
		if isName && redactedNames[normalizeMemberName(tok.String())] {
			if err := dec.SkipValue(); err != nil {
				return nil, err
			}
			if err := enc.WriteToken(jsontext.String(redacted)); err != nil {
				return nil, err
			}
		}
	}
	return bytes.TrimSpace(out.Bytes()), nil
}

func normalizeMemberName(name string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(name))
}

// AuditLog returns audit entries.
func (a *Service) AuditLog(ctx context.Context, q AuditQuery) ([]AuditEntry, int, error) {
	limit := listing.Clamp(q.Limit, 50, 500)
	offset := min(max(q.Offset, 0), auditMaxOffset)
	where, args := "", []any{}
	if s := truncateUTF8(strings.TrimSpace(q.Search), auditMaxSearch); s != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
		where = ` WHERE action LIKE ? ESCAPE '\' OR target LIKE ? ESCAPE '\' OR username LIKE ? ESCAPE '\'
			OR ip LIKE ? ESCAPE '\' OR details LIKE ? ESCAPE '\'`
		args = []any{like, like, like, like, like}
	}
	var total int
	if err := a.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_audit`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := a.db.R.QueryContext(ctx, `SELECT id, time, username, ip, action, target, details FROM auth_audit`+
		where+` ORDER BY time DESC, id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var t int64
		if err := rows.Scan(&e.ID, &t, &e.Username, &e.IP, &e.Action, &e.Target, &e.Details); err != nil {
			return nil, 0, err
		}
		e.Time = db.Time(t)
		out = append(out, e)
	}
	return out, total, rows.Err()
}
