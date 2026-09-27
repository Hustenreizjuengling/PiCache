package settings

import (
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// reconcileChannel keeps updates.channel and its alias includePrereleases
// of versions before 0.15.0 consistent (docs/API.md "Section updates"):
// a changed channel wins (a changed includePrereleases that disagrees
// with it is refused); a changed includePrereleases alone moves stable to
// beta (true; beta and nightly stay) or to stable (false). Every save
// writes includePrereleases = channel != stable.
func (u *Updates) reconcileChannel(old Updates) error {
	u.Channel = strings.ToLower(strings.TrimSpace(u.Channel))
	if u.Channel == "" {
		u.Channel = old.Channel
	}
	chChanged := u.Channel != old.Channel
	inclChanged := u.IncludePrereleases != old.IncludePrereleases
	switch {
	case chChanged && inclChanged && u.IncludePrereleases != (u.Channel != ChannelStable):
		return apperr.Invalid("updates.channel", "includePrereleases contradicts channel: send channel only")
	case !chChanged && inclChanged:
		switch {
		case !u.IncludePrereleases:
			u.Channel = ChannelStable
		case u.Channel == ChannelStable:
			u.Channel = ChannelBeta
		}
	}
	u.IncludePrereleases = u.Channel != ChannelStable
	return nil
}

// validate checks updates.channel.
func (u *Updates) validate() error {
	switch u.Channel {
	case ChannelStable, ChannelBeta, ChannelNightly:
		return nil
	}
	return apperr.Invalid("updates.channel", "must be stable, beta or nightly")
}
