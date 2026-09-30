package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// fillDataDisk makes the test database full for the rows a sign-in and
// the setup store: inserting an account or a session needs pages the
// database can no longer get (SQLITE_FULL, as on a full data disk), while
// reads and in-place updates keep working.
func fillDataDisk(t *testing.T, e *coreEnv) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE fill (b BLOB)`,
		`CREATE TRIGGER fill_sessions BEFORE INSERT ON auth_sessions BEGIN INSERT INTO fill VALUES (zeroblob(1048576)); END`,
		`CREATE TRIGGER fill_users BEFORE INSERT ON auth_users BEGIN INSERT INTO fill VALUES (zeroblob(1048576)); END`,
	} {
		if _, err := e.db.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var pages int
	if err := e.db.W.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	// The writer pool has one connection: the database cannot grow now.
	if _, err := e.db.W.Exec(fmt.Sprintf(`PRAGMA max_page_count = %d`, pages)); err != nil {
		t.Fatal(err)
	}
}

// R2: a sign-in on a full data disk says that the sign-in itself cannot
// be stored and that signed-in browsers and API tokens keep working,
// instead of the generic "the data disk is full" of every write; the
// setup says the same for itself.
func TestSignInOnFullDisk(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	fillDataDisk(t, e)
	w := e.do("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+corePassword+`"}`, "")
	coreWantError(t, w, http.StatusServiceUnavailable, "unavailable", "")
	if !strings.Contains(w.Body.String(), `cannot sign in: PiCache's data disk is full (a sign-in stores a session); `+
		`free space on the host — signed-in browsers and API tokens keep working`) {
		t.Fatalf("login: %s", w.Body)
	}
	if w := e.do("GET", "/api/v1/auth/me", "", session); w.Code != http.StatusOK {
		t.Fatalf("a signed-in browser: %d %s", w.Code, w.Body)
	}
	// A wrong password stays a wrong password.
	w = e.do("POST", "/api/v1/auth/login", `{"username":"admin","password":"wrong password"}`, "")
	coreWantError(t, w, http.StatusUnauthorized, "unauthorized", "")

	e = newCoreEnv(t)
	token := coreReadSetupToken(t, e.setupFile)
	fillDataDisk(t, e)
	w = e.do("POST", "/api/v1/auth/setup", `{"setupToken":"`+token+`","username":"admin","password":"`+corePassword+`"}`, "")
	coreWantError(t, w, http.StatusServiceUnavailable, "unavailable", "")
	if !strings.Contains(w.Body.String(), `cannot finish the setup: PiCache's data disk is full`) {
		t.Fatalf("setup: %s", w.Body)
	}
}
