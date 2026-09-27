package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// Secrets of the settings (sync.token, network.proxy.password) are never
// part of the document: they are sealed with the master key and stored in
// settings_secrets (name, sealed, bound, updated_at) in the same
// transaction as the document. bound is the origin a secret belongs to
// (the sync source's origin; the proxy's origin and user name): a stored
// secret is kept only while it matches, so a changed destination never
// receives it. Input like the notification secrets: absent or null keeps
// the stored secret, "" removes it, a value replaces it. Every read of the
// settings carries only TokenSet and PasswordSet; components that need a
// value call Store.Secret.

// Names of the secrets.
const (
	SecretSyncToken     = "sync.token"
	SecretProxyPassword = "network.proxy.password"
)

// SecretAAD returns the additional data a secret is sealed with.
func SecretAAD(name string) string { return "picache/settings/" + name }

// Sealer seals and opens secrets (the master key's box; app passes it, so
// settings imports only db and apperr).
type Sealer struct {
	Seal func(plaintext []byte, aad string) (string, error)
	Open func(sealed, aad string) ([]byte, error)
}

// SetSealer sets the functions that seal and open secrets. Without them a
// write that stores a secret fails and Secret returns an error.
func (s *Store) SetSealer(seal func([]byte, string) (string, error), open func(string, string) ([]byte, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealer = &Sealer{Seal: seal, Open: open}
}

// secretChange is a pending write of settings_secrets (in persist's
// transaction): sealed "" deletes the row.
type secretChange struct {
	name, sealed, bound string
}

// secretBounds returns the origin each secret of a document belongs to
// ("" when there is nothing it could be sent to).
func secretBounds(a *All) map[string]string {
	src, _ := SyncOrigin(a.Sync.Source)
	if a.Sync.Source == "" {
		src = ""
	}
	return map[string]string{SecretSyncToken: src, SecretProxyPassword: ProxyBound(a.Network.Proxy)}
}

// resolveSecrets applies the secret inputs of next (Sync.Token,
// Network.Proxy.Password) against the stored secrets: it returns the
// writes for persist, sets TokenSet/PasswordSet and removes the inputs
// from the document. A stored secret whose origin changed needs a new
// value (400) unless the new origin is empty (nothing it could be sent
// to: the secret is removed).
func (s *Store) resolveSecrets(next *All) ([]secretChange, map[string]string, error) {
	bounds := secretBounds(next)
	stored := map[string]string{}
	for k, v := range s.bound {
		stored[k] = v
	}
	var changes []secretChange
	for _, sec := range []struct {
		name, field, again, invalid string
		in                          **string
		set                         *bool
		valid                       func(string) bool
		missing                     error
	}{
		{SecretSyncToken, "sync.token", "enter the token again: the primary's address changed", "must be an API token",
			&next.Sync.Token, &next.Sync.TokenSet, ValidSyncToken,
			apperr.Invalid("sync.source", "enter the address of the primary")},
		{SecretProxyPassword, "network.proxy.password", "enter the password again: the proxy changed",
			fmt.Sprintf("at most %d printable characters", MaxProxyPassword),
			&next.Network.Proxy.Password, &next.Network.Proxy.PasswordSet,
			func(v string) bool { return len([]rune(v)) <= MaxProxyPassword && printable(v) },
			apperr.Invalid("network.proxy.url", "set a proxy first")},
	} {
		in := *sec.in
		*sec.in = nil
		bound := bounds[sec.name]
		cur, has := stored[sec.name]
		switch {
		case in != nil && *in == "":
			if has {
				changes = append(changes, secretChange{name: sec.name})
				delete(stored, sec.name)
			}
		case in != nil:
			if !sec.valid(*in) {
				return nil, nil, apperr.Invalid(sec.field, "%s", sec.invalid)
			}
			if bound == "" {
				return nil, nil, sec.missing
			}
			if s.sealer == nil {
				return nil, nil, errors.New("settings: secrets cannot be sealed (no master key)")
			}
			sealed, err := s.sealer.Seal([]byte(*in), SecretAAD(sec.name))
			if err != nil {
				return nil, nil, fmt.Errorf("settings: seal %s: %w", sec.name, err)
			}
			changes = append(changes, secretChange{name: sec.name, sealed: sealed, bound: bound})
			stored[sec.name] = bound
		case has && cur != bound:
			if bound != "" {
				return nil, nil, apperr.Invalid(sec.field, "%s", sec.again)
			}
			changes = append(changes, secretChange{name: sec.name})
			delete(stored, sec.name)
		}
		_, *sec.set = stored[sec.name]
	}
	return changes, stored, nil
}

// loadSecretBounds reads which secrets are stored and their origins.
func loadSecretBounds(ctx context.Context, d *db.DB) (map[string]string, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT name, bound FROM settings_secrets`)
	if err != nil {
		return nil, fmt.Errorf("settings: read secrets: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, bound string
		if err := rows.Scan(&name, &bound); err != nil {
			return nil, err
		}
		out[name] = bound
	}
	return out, rows.Err()
}

// applySecretFlags sets TokenSet and PasswordSet of a loaded document: a
// stored secret counts only while its origin matches the document.
func applySecretFlags(a *All, stored map[string]string) {
	bounds := secretBounds(a)
	flag := func(name string) bool { b, ok := stored[name]; return ok && b == bounds[name] && b != "" }
	a.Sync.Token, a.Network.Proxy.Password = nil, nil
	a.Sync.TokenSet = flag(SecretSyncToken)
	a.Network.Proxy.PasswordSet = flag(SecretProxyPassword)
}

// Secret returns the value of a stored secret ("" when none is stored for
// the current settings: absent, or bound to another origin).
func (s *Store) Secret(ctx context.Context, name string) (string, error) {
	s.mu.Lock()
	sealer := s.sealer
	s.mu.Unlock()
	cur := s.Get()
	want := secretBounds(cur)[name]
	var sealed []byte
	var bound string
	err := s.db.R.QueryRowContext(ctx, `SELECT sealed, bound FROM settings_secrets WHERE name = ?`, name).Scan(&sealed, &bound)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("settings: read %s: %w", name, err)
	}
	if want == "" || bound != want {
		return "", nil
	}
	if sealer == nil {
		return "", errors.New("settings: secrets cannot be opened (no master key)")
	}
	b, err := sealer.Open(string(sealed), SecretAAD(name))
	if err != nil {
		return "", fmt.Errorf("settings: open %s: %w", name, err)
	}
	return string(b), nil
}

// writeSecrets applies pending secret writes in tx.
func writeSecrets(ctx context.Context, tx *sql.Tx, changes []secretChange) error {
	for _, c := range changes {
		var err error
		if c.sealed == "" {
			_, err = tx.ExecContext(ctx, `DELETE FROM settings_secrets WHERE name = ?`, c.name)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO settings_secrets (name, sealed, bound, updated_at) VALUES (?, ?, ?, ?)
				ON CONFLICT(name) DO UPDATE SET sealed = excluded.sealed, bound = excluded.bound, updated_at = excluded.updated_at`,
				c.name, []byte(c.sealed), c.bound, db.NowMs())
		}
		if err != nil {
			return fmt.Errorf("settings: save %s: %w", c.name, err)
		}
	}
	return nil
}
