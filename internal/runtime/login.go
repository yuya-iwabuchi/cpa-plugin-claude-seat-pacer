package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

// loginConfirm is how long a token stays inside the host's refresh lead, on
// the plugin's clock, before the plugin takes its renewals to be failing. The
// host checks every few seconds, so a token it renews late only because the
// machine slept leaves the lead well inside this.
const loginConfirm = 10 * time.Minute

// loginFile is the part of a Claude credential file that describes its login.
// It holds no token.
type loginFile struct {
	Expired          string          `json:"expired"`
	LastRefresh      string          `json:"last_refresh"`
	OrganizationName string          `json:"organization_name"`
	DeviceIDs        json.RawMessage `json:"claude_device_ids"`
}

func parseLoginFile(raw json.RawMessage) (loginFile, bool) {
	var f loginFile
	if json.Unmarshal(raw, &f) != nil || f.Expired == "" {
		return loginFile{}, false
	}
	return f, true
}

// deviceDigest digests the device id a Claude credential file holds, empty
// when it holds no valid one: the first entry that is 64 hex digits once
// trimmed and lowercased, as the host reads it. The id is random, so its
// digest discloses nothing.
func deviceDigest(raw json.RawMessage) string {
	var ids []any
	if json.Unmarshal(raw, &ids) != nil {
		return ""
	}
	for _, v := range ids {
		id, _ := v.(string)
		id = strings.ToLower(strings.TrimSpace(id))
		if _, err := hex.DecodeString(id); err != nil || len(id) != 64 {
			continue
		}
		sum := sha256.Sum256([]byte(id))
		return hex.EncodeToString(sum[:8])
	}
	return ""
}

// loginTrack is one seat's login as successive polls saw it.
type loginTrack struct {
	expires     time.Time
	lastRefresh time.Time
	team        string
	// device is the file's deviceDigest.
	device string
	// refused is whether the host has listed the current refresh token
	// refused since its last renewal.
	refused bool
	// newDevice is whether the latest reading found a new device id. The
	// host's listing is read before the file, so it may still describe the
	// login the new one replaced.
	newDevice bool
	// overdueSince is when the plugin first saw the current token inside the
	// host's refresh lead; zero while it is outside.
	overdueSince time.Time
	// seenAt is when the plugin last read the file.
	seenAt time.Time
	// loggedInAt is when the plugin saw the current login made; zero when it
	// was made before the plugin was watching.
	loggedInAt time.Time
	// reported is the state reportLogins last saw for the seat, empty before
	// the first, and reportedLogin the login it last saw.
	reported      string
	reportedLogin time.Time
}

// observeLogin folds one reading of a credential file, and the host's refusal
// of its refresh token, into its track. A file whose device id changed from
// one valid id to another was replaced by a new login. Without an id on both
// readings it falls back on timing: a renewal replaces a token about
// model.LoginRefreshLead before it runs out, so a token replaced well before
// that, or one replaced after its renewals had been failing, was replaced by
// a new login. kept reports a renewal of a refresh token the host had listed
// refused, under the same device id, which no login the host makes produces.
// The host keeps an invalid-grant status through the file a login writes, and
// sets it only on a token that has run out, so it counts only then.
func observeLogin(prev *loginTrack, f loginFile, r refusal, now time.Time) (next loginTrack, kept, ok bool) {
	expires, err := time.Parse(time.RFC3339, f.Expired)
	if err != nil {
		return loginTrack{}, false, false
	}
	refreshed, _ := time.Parse(time.RFC3339, f.LastRefresh)
	next = loginTrack{expires: expires, lastRefresh: refreshed, team: strings.TrimSpace(f.OrganizationName), device: deviceDigest(f.DeviceIDs), seenAt: now}
	next.refused = r == refusalRevoked || r == refusalGrant && !expires.After(now)
	if prev != nil {
		next.loggedInAt, next.reported, next.reportedLogin = prev.loggedInAt, prev.reported, prev.reportedLogin
		if expires.Equal(prev.expires) {
			next.overdueSince = prev.overdueSince
		}
		renewed := refreshed.After(prev.lastRefresh)
		switch {
		case prev.device != "" && next.device != "" && prev.device != next.device:
			next.newDevice, next.refused = true, false
			next.loggedInAt = time.Time{}
			if renewed {
				next.loggedInAt = refreshed
			}
		case prev.device != "" && next.device != "":
			kept = renewed && prev.refused
			next.refused = next.refused || prev.refused && !renewed
		case renewed:
			early := prev.expires.Sub(refreshed) > model.LoginRefreshLead+loginConfirm
			if early || prev.confirmedAt(refreshed) {
				next.loggedInAt = refreshed
			}
		}
	}
	if expires.Sub(now) >= model.LoginRefreshLead {
		next.overdueSince = time.Time{}
	} else if next.overdueSince.IsZero() {
		next.overdueSince = now
	}
	return next, kept, true
}

// confirmedAt reports whether readings up to at had kept the token inside the
// refresh lead for loginConfirm. A seat not read since stays as last read.
func (t loginTrack) confirmedAt(at time.Time) bool {
	if t.seenAt.Before(at) {
		at = t.seenAt
	}
	return !t.overdueSince.IsZero() && at.Sub(t.overdueSince) >= loginConfirm
}

// refusal is what the host's listing says of a credential's refresh token.
type refusal int

const (
	refusalNone refusal = iota
	// refusalGrant is a refresh token the token endpoint refused once the
	// access token had run out, which the host retries.
	refusalGrant
	// refusalRevoked is a refresh token refused after Anthropic rejected the
	// access token too: the host has stopped using the seat.
	refusalRevoked
)

// hostRefusal reads the host's status message for a credential.
func hostRefusal(entry HostAuthFileEntry) refusal {
	msg := strings.ToLower(entry.StatusMessage)
	switch {
	case strings.Contains(msg, "refresh token invalid"):
		return refusalRevoked
	case strings.Contains(msg, "invalid grant") || strings.Contains(msg, "invalid_grant"):
		return refusalGrant
	}
	return refusalNone
}

// login is the track's state at now, given the host's refusal.
func (t loginTrack) login(now time.Time, r refusal) *model.Login {
	exp := t.expires
	l := &model.Login{State: model.LoginOK, Team: t.team, AccessExpiresAt: &exp}
	// A login still renewing past its expected end was replaced unseen, or
	// lasts longer than expected: either way its end is unknown.
	if end := t.loggedInAt.Add(model.LoginLifetime); !t.loggedInAt.IsZero() && end.After(now) {
		at := t.loggedInAt
		l.LoggedInAt, l.EndsAround = &at, &end
	}
	dead := !t.expires.After(now)
	switch {
	case r == refusalRevoked || dead && (r == refusalGrant || t.confirmedAt(now)):
		l.State = model.LoginExpired
	case !dead && t.confirmedAt(now):
		l.State = model.LoginFailing
	case l.EndsAround != nil && l.EndsAround.Sub(now) < model.LoginAgeingLead:
		l.State = model.LoginAgeing
	}
	return l
}

// seatLogins is the login of each listed, enabled seat the polls have read,
// keyed by credential id. While every such seat's last usage read failed to
// reach Anthropic, the host is cut off, as during a network outage: a seat
// whose renewals look failing, or whose token ran out with no refusal from
// the host, is stalled rather than at fault.
func seatLogins(entries []HostAuthFileEntry, tracks map[string]loginTrack, polls map[string]pollState, now time.Time) map[string]*model.Login {
	out := make(map[string]*model.Login, len(tracks))
	refused := make(map[string]bool, len(tracks))
	cut := true
	for _, entry := range entries {
		id := authID(entry)
		track, ok := tracks[id]
		if !ok || entry.Disabled {
			continue
		}
		r := hostRefusal(entry)
		if track.newDevice {
			r = refusalNone
		}
		out[id] = track.login(now, r)
		refused[id] = r != refusalNone
		category := polls[id].category
		cut = cut && (category == string(quota.CategoryTransport) || category == string(quota.CategoryTimeout))
	}
	if !cut {
		return out
	}
	for id, l := range out {
		if l.State == model.LoginFailing || l.State == model.LoginExpired && !refused[id] {
			l.State = model.LoginStalled
		}
	}
	return out
}
