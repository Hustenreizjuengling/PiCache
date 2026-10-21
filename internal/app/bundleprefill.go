package app

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

// bundlePrefill is prefill.json of the support bundle: whether the
// prefill service runs, its status without the accounts (names, IDs and
// sign-in codes are left out; the account names become placeholders in
// the log lines too), its last unexpected ends and the end of its own
// log. The service never has a token in either.
type bundlePrefill struct {
	Installed bool               `json:"installed"`
	Running   bool               `json:"running"`
	Error     string             `json:"error,omitempty"`
	Status    *prefillapi.Status `json:"status,omitempty"`
	Log       []string           `json:"log,omitempty"`
}

const bundlePrefillLines = 300

func (a *App) bundlePrefill(ctx context.Context, sc *scrubber) bundlePrefill {
	c := prefillapi.NewClient(a.cfg.PrefillSocket)
	out := bundlePrefill{Installed: c.Installed()}
	if !out.Installed {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := c.Status(ctx)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Running = true
	if acc := st.Steam.Account; acc != nil {
		sc.addName(acc.Name)
		acc.Name, acc.SteamID = "(account)", ""
	}
	if l := st.Steam.Login; l != nil {
		l.ChallengeURL = ""
	}
	if acc := st.Epic.Account; acc != nil {
		sc.addName(acc.Name)
		acc.Name, acc.AccountID = "(account)", ""
	}
	if l := st.Epic.Login; l != nil {
		l.VerificationURL, l.UserCode = "", ""
	}
	out.Status = &st
	if r, err := c.Do(ctx, http.MethodGet, "/v1/log?lines="+strconv.Itoa(bundlePrefillLines), nil); err == nil && r.Status == http.StatusOK {
		var lg prefillapi.Log
		if json.Unmarshal(r.Body, &lg) == nil {
			out.Log = lg.Lines
		}
	}
	return out
}
