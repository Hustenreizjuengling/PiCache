package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
)

// Create adds a target (ValidateTarget).
func (m *Manager) Create(ctx context.Context, in TargetInput) (Target, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	n := len(m.targets)
	m.mu.Unlock()
	if n >= maxTargets {
		return Target{}, apperr.Conflict("at most %d storage targets are supported", maxTargets)
	}
	t := m.fromInput(newID(), in)
	sealed, _, err := m.passwordFor(t, in.Password)
	if err != nil {
		return Target{}, err
	}
	t.HasPassword = sealed != ""
	if err := ValidateTarget(t, m.cfg); err != nil {
		return Target{}, err
	}
	if err := m.checkOverlap(t); err != nil {
		return Target{}, err
	}
	now := db.Time(db.NowMs())
	t.CreatedAt, t.UpdatedAt = now, now
	if err := insertTarget(ctx, m.db.W, t, sealed); err != nil {
		return Target{}, err
	}
	m.mu.Lock()
	m.targets[t.ID] = &entry{t: t, st: pendingStatus(t, "not checked yet")}
	m.mu.Unlock()
	m.log.Info("storage target created", slog.String("target", t.ID), slog.String("kind", string(t.Kind)),
		slog.String("mode", string(t.Mode)), slog.String("path", t.Path))
	m.kickGuard()
	return t, nil
}

// Update changes a target (ValidateTarget). The built-in local target only
// allows renaming.
func (m *Manager) Update(ctx context.Context, id string, in TargetInput) (Target, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	cur, ok := m.get(id)
	if !ok {
		return Target{}, apperr.NotFound("storage target", id)
	}
	t := cur
	var sealed string
	var setPassword bool
	if id == LocalTargetID {
		t.Name = strings.TrimSpace(in.Name)
	} else {
		t = m.fromInput(id, in)
		t.StoreID, t.CreatedAt, t.HasPassword = cur.StoreID, cur.CreatedAt, cur.HasPassword
		var err error
		if sealed, setPassword, err = m.passwordFor(t, in.Password); err != nil {
			return Target{}, err
		}
		if setPassword {
			t.HasPassword = sealed != ""
		}
	}
	if err := ValidateTarget(t, m.cfg); err != nil {
		return Target{}, err
	}
	if err := m.checkOverlap(t); err != nil {
		return Target{}, err
	}
	t.UpdatedAt = db.Time(db.NowMs())
	if err := updateTarget(ctx, m.db.W, t, sealed, setPassword); err != nil {
		return Target{}, err
	}

	m.mu.Lock()
	e := m.targets[id]
	relocated := e != nil && !sameLocation(e.t, t)
	var st Status
	if e != nil {
		e.t = t
		if relocated {
			e.gen++
			e.st, e.uninit = pendingStatus(t, "configuration changed; checking again"), false
			e.notified, st = e.st, e.st
		}
	}
	m.mu.Unlock()
	m.log.Info("storage target updated", slog.String("target", id), slog.Bool("relocated", relocated),
		slog.Bool("passwordChanged", setPassword && t.Kind == KindSMB))
	if cur.Mode == ModeHostApply && t.Mode != ModeHostApply {
		m.queueRemoval(id) // the root helper no longer manages this mount
	}
	if relocated {
		m.notify(id, st)
		m.kickGuard()
	}
	return t, nil
}

// Delete removes a target (not "local", not the active one).
func (m *Manager) Delete(ctx context.Context, id, activeID string) error {
	if id == LocalTargetID {
		return apperr.Forbidden("the built-in local storage cannot be deleted")
	}
	if id == activeID {
		return apperr.Conflict("the active storage target cannot be deleted; activate another one first")
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	t, ok := m.get(id)
	if !ok {
		return apperr.NotFound("storage target", id)
	}
	if _, err := m.db.W.ExecContext(ctx, `DELETE FROM storage_targets WHERE id = ?`, id); err != nil {
		return fmt.Errorf("storage: delete target: %w", err)
	}
	m.mu.Lock()
	delete(m.targets, id)
	m.mu.Unlock()
	m.log.Info("storage target deleted", slog.String("target", id))
	// The mount unit and the credentials of a host-apply target (or of one
	// whose removal is still outstanding) are removed by the root helper.
	if dir := requestsDir(m.cfg); t.Mode == ModeHostApply || removalOutstanding(dir, id) {
		m.queueRemoval(id)
	} else {
		removeRequestFiles(dir, id)
	}
	return nil
}

// Test runs a full functional test (mount guard, write/rename/read/delete in
// tmp/, statfs) without changing anything else.
func (m *Manager) Test(ctx context.Context, id string) TestResult {
	m.mu.Lock()
	e := m.targets[id]
	busy := e != nil && e.busy
	m.mu.Unlock()
	switch {
	case e == nil:
		return TestResult{Error: fmt.Sprintf("storage target %s not found", id), Steps: []string{}}
	case busy: // the write test must not race the emptiness check of InitStore
		return TestResult{Error: "the store is being initialised; test again in a moment", Steps: []string{}, Status: m.Status(id)}
	}
	res, err := m.freshCheck(ctx, id, testTimeout)
	if err != nil {
		return TestResult{Steps: []string{"Started the check"}, Error: errMessage(err),
			Hint: "The NAS or the network does not respond; PiCache serves downloads uncached meanwhile.", Status: m.Status(id)}
	}
	tr := TestResult{OK: res.usable(), Steps: res.steps, Hint: res.st.Hint, Status: res.st}
	if !tr.OK {
		tr.Error = res.st.Reason
	}
	return tr
}

// InitStore writes the store marker into an empty, guarded root (adopt=false),
// or adopts an existing marker (adopt=true) and records its store id.
func (m *Manager) InitStore(ctx context.Context, id string, adopt bool) (InitResult, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	t, ok := m.get(id)
	if !ok {
		return InitResult{}, apperr.NotFound("storage target", id)
	}
	m.setBusy(id, true)
	defer m.setBusy(id, false)

	res, err := m.freshCheck(ctx, id, initTimeout)
	if err != nil {
		return InitResult{}, err
	}
	if !res.located || (!res.rootMissing && !res.writable) {
		return InitResult{}, unavailable(res.st)
	}
	storeID, err := m.initFS(ctx, t, adopt, res.rootMissing)
	if err != nil {
		return InitResult{}, err
	}
	if owner := m.storeOwner(storeID, id); owner != "" {
		return InitResult{}, apperr.Conflict("this store is already used by the storage target %q", owner)
	}
	if err := m.setStoreID(ctx, id, storeID); err != nil {
		return InitResult{}, err
	}
	verb := "initialised"
	if adopt {
		verb = "adopted"
	}
	m.log.Info("cache store "+verb, slog.String("target", id), slog.String("store", storeID))
	m.setBusy(id, false)
	if _, err := m.freshCheck(ctx, id, initTimeout); err != nil {
		m.log.Warn("storage check after initialisation failed", slog.String("target", id), slog.Any("err", err))
	}
	return InitResult{StoreID: storeID, Adopted: adopt}, nil
}

// initFS creates or reads the store marker in a goroutine with a timeout (a
// hung NAS must not hang the caller).
func (m *Manager) initFS(ctx context.Context, t Target, adopt, createRoot bool) (string, error) {
	type result struct {
		id  string
		err error
	}
	ch := make(chan result, 1)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errShuttingDown
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		id, err := m.initRoot(t, adopt, createRoot)
		ch <- result{id, err}
	}()
	timer := time.NewTimer(initTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.id, r.err
	case <-timer.C:
		return "", apperr.Unavailable("the storage does not respond (no answer within %s)", initTimeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (m *Manager) initRoot(t Target, adopt, createRoot bool) (string, error) {
	root := storeRootPath(t)
	if createRoot {
		if adopt {
			return "", apperr.Conflict("the store directory %s does not exist; there is nothing to adopt", root)
		}
		dir, err := os.OpenRoot(t.Path)
		if err != nil {
			return "", m.fsError("cannot open "+t.Path, err)
		}
		err = dir.MkdirAll(filepath.FromSlash(t.Subdir), 0o750)
		dir.Close()
		if err != nil {
			return "", m.fsError("cannot create the store directory", err)
		}
	}
	mk, err := cachestore.ReadMarker(root)
	if adopt {
		switch {
		case errors.Is(err, cachestore.ErrNoMarker):
			return "", apperr.Conflict("no PiCache store was found in %s; initialise it instead", root)
		case err != nil:
			return "", apperr.Wrap(apperr.KindConflict, err, "the existing store marker cannot be used: %s", err.Error())
		}
		return mk.StoreID, nil
	}
	switch {
	case err == nil:
		return "", apperr.Conflict("a PiCache store (%s) already exists in %s; adopt it instead", mk.StoreID, root)
	case !errors.Is(err, cachestore.ErrNoMarker):
		return "", apperr.Wrap(apperr.KindConflict, err, "an unusable store marker exists in %s: %s", root, err.Error())
	}
	mk, err = cachestore.InitRoot(root, newID(), m.sliceSize())
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return "", m.fsError("cannot write the store marker", err)
		}
		// Not empty, invalid slice size: messages are written for the admin.
		return "", apperr.Wrap(apperr.KindConflict, err, "%s", err.Error())
	}
	return mk.StoreID, nil
}

// fsError maps a file system error to an Unavailable error with a hint.
func (m *Manager) fsError(what string, err error) error {
	msg := what + ": " + err.Error()
	if hint := m.writeHint(err); hint != "" {
		msg += ". " + hint
	}
	return apperr.Wrap(apperr.KindUnavailable, err, "%s", msg)
}

// storeOwner returns the name of another target that records storeID.
func (m *Manager) storeOwner(storeID, except string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.targets {
		if id != except && e.t.StoreID == storeID {
			return e.t.Name
		}
	}
	return ""
}

// setStoreID records the store id of a target.
func (m *Manager) setStoreID(ctx context.Context, id, storeID string) error {
	now := db.NowMs()
	if _, err := m.db.W.ExecContext(ctx, `UPDATE storage_targets SET store_id = ?, updated_at = ? WHERE id = ?`,
		storeID, now, id); err != nil {
		return fmt.Errorf("storage: record store id: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.targets[id]; e != nil {
		e.t.StoreID, e.t.UpdatedAt = storeID, db.Time(now)
		e.gen++
	}
	return nil
}

func (m *Manager) setBusy(id string, busy bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.targets[id]; e != nil {
		e.busy = busy
	}
}

// checkOverlap refuses store roots that equal or nest with another target's.
func (m *Manager) checkOverlap(t Target) error {
	root := storeRootPath(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.targets {
		if id != t.ID && overlaps(root, storeRootPath(e.t)) {
			return apperr.Conflict("the store location overlaps with the storage target %q", e.t.Name)
		}
	}
	return nil
}

// fromInput normalises an input: trims, canonicalises the server address,
// applies defaults, clears fields that do not apply to the kind and computes
// the host-apply path.
func (m *Manager) fromInput(id string, in TargetInput) Target {
	t := Target{
		ID:                id,
		Name:              strings.TrimSpace(in.Name),
		Kind:              Kind(strings.ToLower(strings.TrimSpace(string(in.Kind)))),
		Mode:              Mode(strings.ToLower(strings.TrimSpace(string(in.Mode)))),
		Path:              strings.TrimSpace(in.Path),
		Subdir:            strings.Trim(strings.TrimSpace(in.Subdir), "/"),
		RequireMountpoint: in.RequireMountpoint,
	}
	if t.Mode == "" {
		t.Mode = ModeExternal
	}
	if t.Path != "" {
		t.Path = filepath.Clean(t.Path)
	}
	server := strings.TrimSpace(in.Server)
	if a, err := netip.ParseAddr(server); err == nil && a.Zone() == "" {
		server = a.String()
	}
	switch t.Kind {
	case KindSMB:
		t.Server, t.Share = server, strings.TrimSpace(in.Share)
		t.Username, t.Domain = strings.TrimSpace(in.Username), strings.TrimSpace(in.Domain)
		t.SMBVersion, t.SMBSeal = strings.TrimSpace(in.SMBVersion), in.SMBSeal
		if t.SMBVersion == "" {
			t.SMBVersion = smbVersions[0]
		}
		t.RequireMountpoint = true
	case KindNFS:
		t.Server, t.Export = server, strings.TrimSpace(in.Export)
		t.NFSVersion, t.NFSNConnect = strings.TrimSpace(in.NFSVersion), in.NFSNConnect
		if t.NFSVersion == "" {
			t.NFSVersion = nfsVersions[0]
		}
		if t.NFSNConnect == 0 {
			t.NFSNConnect = defNConnect
		}
		t.RequireMountpoint = true
	}
	if t.Mode == ModeHostApply {
		t.Path = hostApplyPath(m.cfg, id)
	}
	return t
}

// passwordFor returns the sealed password to store and whether the column
// changes (in == nil keeps it). Only SMB targets with a username keep one.
func (m *Manager) passwordFor(t Target, in *string) (sealed string, set bool, err error) {
	if t.Kind != KindSMB {
		if in != nil && *in != "" {
			return "", false, apperr.Invalid("password", "only SMB targets use a password")
		}
		return "", true, nil
	}
	if in == nil {
		if t.Username == "" {
			return "", true, nil // guest access never keeps a password
		}
		return "", false, nil
	}
	if *in == "" {
		return "", true, nil
	}
	if err := validatePassword(*in); err != nil {
		return "", false, err
	}
	if t.Username == "" {
		return "", false, apperr.Invalid("password", "set a username as well (leave both empty for guest access)")
	}
	if m.box == nil {
		return "", false, errors.New("storage: no master key to seal the password")
	}
	s, err := m.box.Seal([]byte(*in), passwordAAD(t.ID))
	if err != nil {
		return "", false, fmt.Errorf("storage: seal password: %w", err)
	}
	return s, true, nil
}

// sameLocation reports whether two versions of a target point to the same
// storage (everything except name, timestamps and password).
func sameLocation(a, b Target) bool {
	a.Name, b.Name = "", ""
	a.HasPassword, b.HasPassword = false, false
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	return a == b
}

// unavailable turns an offline status into an API error.
func unavailable(st Status) error {
	msg := st.Reason
	if msg == "" {
		msg = "the storage is not usable"
	}
	if st.Hint != "" {
		msg += ". " + st.Hint
	}
	return &apperr.Error{Kind: apperr.KindUnavailable, Message: msg}
}

// errMessage returns the user-facing text of an error.
func errMessage(err error) string {
	if ae, ok := apperr.As(err); ok {
		return ae.Error()
	}
	return err.Error()
}
