package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

// Reasons a reset claim or a seat recheck is not sent. Each is
// model.ErrClaimNotSent to errors.Is.
var (
	ErrNoSuchSeat     error = notSent("the plugin does not read that seat")
	ErrSeatUnreadable error = notSent("the host did not hand over the seat's credential")
	ErrNoOrganization error = notSent("the seat's credential names no organization")
)

// ErrClaimBusy answers a claim while another runs. It is not
// model.ErrClaimNotSent: the claim running may be this one, sent again by a
// page that lost its answer.
var ErrClaimBusy = errors.New("another reset is being used; try again once it answers")

type notSent string

func (e notSent) Error() string        { return string(e) }
func (e notSent) Is(target error) bool { return target == model.ErrClaimNotSent }

// MinSeatRecheckGap is the least time between two rechecks of one seat. The
// status page rechecks a seat a few times while it waits for a reset to
// show, and every open page can ask. The usage endpoint refuses reads of one
// credential that come a few seconds apart.
const MinSeatRecheckGap = 15 * time.Second

// ClaimReset uses one of grantID's limit resets on the seat whose credential
// id is seat, as the status page asks, then reads the seat again as
// RecheckSeat does unless a poll or a recheck is running, so the page shows
// what the claim did. requestID names the claim: the endpoint answers a request id it has
// already seen with its first answer, so a claim the page retries with the
// same id uses at most one reset.
//
// It reads the seat's token and organization from the credential file the
// host holds, and logs neither. One claim runs at a time: a second answers
// ErrClaimBusy rather than waiting. The pick never waits on a claim.
func (p *Plugin) ClaimReset(ctx context.Context, seat, grantID, requestID string) (model.ResetClaim, error) {
	if !p.claimMu.TryLock() {
		return model.ResetClaim{}, ErrClaimBusy
	}
	defer p.claimMu.Unlock()

	entry, ok := p.seatEntry(seat)
	if !ok {
		return model.ResetClaim{}, ErrNoSuchSeat
	}
	auth, err := p.seatAuth(ctx, entry)
	if err != nil {
		return model.ResetClaim{}, err
	}
	var file struct {
		AccessToken      string `json:"access_token"`
		OrganizationUUID string `json:"organization_uuid"`
	}
	if json.Unmarshal(auth.JSON, &file) != nil || file.AccessToken == "" {
		return model.ResetClaim{}, ErrSeatUnreadable
	}
	if file.OrganizationUUID == "" {
		return model.ResetClaim{}, ErrNoOrganization
	}

	client := p.usageClient()
	fields := map[string]any{"auth_id": seat, "grant_id": grantID}
	res, err := client.ClaimReset(ctx, file.AccessToken, file.OrganizationUUID, grantID, requestID)
	if err != nil {
		fields["error"] = err.Error()
		p.host.log("warn", "claude-seat-pacer could not use a limit reset", fields)
		return model.ResetClaim{}, err
	}
	fields["result"] = res.Result
	if res.Reason != "" {
		fields["reason"] = res.Reason
	}
	p.host.log("info", "claude-seat-pacer asked to use a limit reset", fields)

	if p.tryRecheckLock() {
		defer p.recheckUnlock()
		p.markRechecked(seat)
		_ = p.reread(ctx, client, seat, entry, auth)
	}
	return res, nil
}

// RecheckSeat reads the seat whose credential id is seat again, as the status
// page asks while it waits for a used reset to show. It reports whether it
// read: it does not while a poll runs, which reads the seat itself, while
// another seat's recheck runs, or within MinSeatRecheckGap of the seat's last
// recheck.
//
// Beside the usage, it takes the seat's entry from a fresh host listing and
// asks the host to clear a cooldown the reading shows stale, as a poll does,
// so a seat a reset emptied is offered again without waiting for the next
// poll. A read that fails leaves the cooldown alone and returns its error.
func (p *Plugin) RecheckSeat(ctx context.Context, seat string) (bool, error) {
	entry, ok := p.seatEntry(seat)
	if !ok {
		return false, ErrNoSuchSeat
	}
	if !p.tryRecheckLock() {
		return false, nil
	}
	defer p.recheckUnlock()
	if !p.markRechecked(seat) {
		return false, nil
	}
	auth, err := p.seatAuth(ctx, entry)
	if err != nil {
		return false, err
	}
	if err := p.reread(ctx, p.usageClient(), seat, entry, auth); err != nil {
		return false, err
	}
	return true, nil
}

// tryRecheckLock takes pollMu for one seat's read unless a poll or another
// recheck holds it. The poll lock keeps a recheck from publishing a listing
// and holds that a running poll would then replace with ones it read before.
// SyncNow waits out a recheck rather than answering that a poll is running.
func (p *Plugin) tryRecheckLock() bool {
	if !p.pollMu.TryLock() {
		return false
	}
	p.rechecking.Store(true)
	return true
}

func (p *Plugin) recheckUnlock() {
	p.rechecking.Store(false)
	p.pollMu.Unlock()
}

// markRechecked records a recheck of seat now, unless the last one was
// within MinSeatRecheckGap, and reports whether it did.
func (p *Plugin) markRechecked(seat string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if last, ok := p.rechecked[seat]; ok && now.Sub(last) < MinSeatRecheckGap {
		return false
	}
	if p.rechecked == nil {
		p.rechecked = make(map[string]time.Time)
	}
	p.rechecked[seat] = now
	return true
}

// seatEntry is the last poll's listing entry for seat, for a seat the plugin
// reads.
func (p *Plugin) seatEntry(seat string) (HostAuthFileEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.auths {
		if authID(e) == seat {
			return e, !e.Disabled && !e.RuntimeOnly && e.AuthIndex != ""
		}
	}
	return HostAuthFileEntry{}, false
}

// seatAuth is the seat's credential file as the host holds it.
func (p *Plugin) seatAuth(ctx context.Context, entry HostAuthFileEntry) (HostAuthGetResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, refreshHeadroom)
	defer cancel()
	auth, err := p.host.authGet(ctx, entry.AuthIndex)
	if err != nil {
		return HostAuthGetResponse{}, ErrSeatUnreadable
	}
	return auth, nil
}

func (p *Plugin) usageClient() *quota.Client {
	cfg := p.config()
	return quota.NewClient(hostDoer{h: p.host}, cfg.Quota.UsageURL, cfg.Quota.RequestTimeout)
}

// reread reads one seat's usage into the store and, when the read succeeds,
// takes the seat's entry from a fresh host listing and settles its cooldown
// as releaseHold does. The caller holds pollMu.
func (p *Plugin) reread(ctx context.Context, client *quota.Client, seat string, entry HostAuthFileEntry, auth HostAuthGetResponse) error {
	ctx, cancel := context.WithTimeout(ctx, p.config().Quota.RequestTimeout+refreshHeadroom)
	defer cancel()
	snap, err := p.fetchOne(ctx, client, seat, entry, auth, nil)
	if snap.AuthID == "" {
		return nil
	}
	p.quota.Put(snap)
	if err != nil {
		return err
	}
	p.releaseHold(ctx, p.relist(ctx, entry))
	return nil
}

// relist is seat's entry from a fresh host listing, stored over the last
// poll's, or entry itself when the listing fails or leaves the seat out.
// Only this seat's entry changes: the rest wait for the next poll.
func (p *Plugin) relist(ctx context.Context, entry HostAuthFileEntry) HostAuthFileEntry {
	entries, err := p.host.authList(ctx)
	if err != nil {
		return entry
	}
	id := authID(entry)
	for _, e := range entries {
		if authID(e) != id {
			continue
		}
		p.mu.Lock()
		p.auths = withEntry(p.auths, id, func(x *HostAuthFileEntry) { *x = e })
		p.mu.Unlock()
		return e
	}
	return entry
}

// withEntry is a copy of auths with change applied to the entry for id. The
// poll goroutine reads the slice it stored without holding mu, so a write
// from outside the poll replaces the slice rather than writing in place.
func withEntry(auths []HostAuthFileEntry, id string, change func(*HostAuthFileEntry)) []HostAuthFileEntry {
	out := append([]HostAuthFileEntry(nil), auths...)
	for i := range out {
		if authID(out[i]) == id {
			change(&out[i])
		}
	}
	return out
}
