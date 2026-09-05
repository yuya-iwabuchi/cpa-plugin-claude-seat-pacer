package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/quota"
)

// Poll scheduling. The first poll waits for the host to finish wiring its auth
// manager, which is not guaranteed to have happened when plugin.register
// returns; a poll that cannot list credentials retries sooner than the
// configured interval.
const (
	startupGrace  = 2 * time.Second
	listRetryWait = 15 * time.Second
)

// hostDrainTimeout bounds the wait for host callbacks still parked in the host
// once the poll loop has stopped.
const hostDrainTimeout = 5 * time.Second

// poller drives the periodic poll. It is stopped and joined at plugin.quiesce
// and plugin.shutdown; a management refresh runs the same poll inline on its
// own goroutine instead.
type poller struct {
	stop chan struct{}
	done chan struct{}
}

// startPoller starts the poll loop once; later calls are no-ops, which is what
// lets plugin.reconfigure share a handler with plugin.register.
func (p *Plugin) startPoller() {
	p.lifeMu.Lock()
	defer p.lifeMu.Unlock()
	if p.poller != nil {
		return
	}
	pl := &poller{stop: make(chan struct{}), done: make(chan struct{})}
	p.poller = pl
	go p.runPoller(pl)
}

// Shutdown stops the poll loop, waits for it, and then drains the host calls
// it left outstanding. It is idempotent, and a later plugin.register starts a
// fresh loop.
//
// The drain is what makes the unload safe: the host frees its callback table
// and dlcloses this library as soon as cliproxy_plugin_shutdown returns, so a
// goroutine still parked in a host call would resume in unmapped memory.
func (p *Plugin) Shutdown() {
	p.lifeMu.Lock()
	defer p.lifeMu.Unlock()
	if p.poller != nil {
		close(p.poller.stop)
		<-p.poller.done
		p.poller = nil
	}
	p.host.drain(hostDrainTimeout)
}

func (p *Plugin) runPoller(pl *poller) {
	defer close(pl.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-pl.stop
		cancel()
	}()

	timer := time.NewTimer(p.startDelay)
	defer timer.Stop()
	for {
		select {
		case <-pl.stop:
			return
		case <-timer.C:
		}
		timer.Reset(p.poll(ctx))
	}
}

// refresh polls every governed credential now and reports the first failure:
// a credential listing that failed, a usage fetch that failed, or a context
// that expired part-way. It runs inline on the caller's goroutine, so a
// management request that triggers it sees the refreshed state in its own
// response.
func (p *Plugin) refresh(ctx context.Context) error {
	p.poll(ctx)
	p.mu.Lock()
	listErr, fetchErr := p.listErr, p.fetchErr
	p.mu.Unlock()
	switch {
	case listErr != "":
		return errPoll(listErr)
	case fetchErr != "":
		return errPoll(fetchErr)
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return nil
}

type errPoll string

func (e errPoll) Error() string { return string(e) }

// poll reads every governed OAuth credential's usage once and returns how long
// to wait before the next poll. Polls are serialized so a manual refresh
// cannot overlap the loop.
func (p *Plugin) poll(ctx context.Context) time.Duration {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()

	cfg := p.config()
	if !cfg.Enabled {
		p.mu.Lock()
		p.listErr, p.fetchErr = "", ""
		p.mu.Unlock()
		return cfg.Quota.PollInterval
	}
	p.warnSingleCandidate()

	entries, err := p.host.authList(ctx)
	if err != nil {
		p.mu.Lock()
		p.listErr = err.Error()
		p.mu.Unlock()
		p.host.log("warn", "cpa-claude-quota-scheduler could not list credentials", map[string]any{"error": err.Error()})
		return listRetryWait
	}

	governed := make([]HostAuthFileEntry, 0, len(entries))
	listed := make(map[string]struct{}, len(entries))
	keep := make(map[string]struct{}, len(entries))
	fetchErr := ""
	client := quota.NewClient(hostDoer{h: p.host}, cfg.Quota.UsageURL, cfg.Quota.RequestTimeout)
	for _, entry := range entries {
		if !cfg.GovernsProvider(strings.ToLower(entry.Provider)) && !cfg.GovernsProvider(strings.ToLower(entry.Type)) {
			continue
		}
		governed = append(governed, entry)
		id := authID(entry)
		if id != "" {
			listed[id] = struct{}{}
		}
		if entry.Disabled || entry.RuntimeOnly {
			// The host never offers a disabled credential, and a runtime-only
			// one has no file for host.auth.get to read.
			continue
		}
		if id == "" || entry.AuthIndex == "" {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		snap, err := p.fetchOne(ctx, client, id, entry)
		if snap.AuthID == "" {
			// Not an OAuth credential: nothing to poll and nothing to route on.
			continue
		}
		keep[id] = struct{}{}
		p.quota.Put(snap)
		p.mu.Lock()
		state := pollState{at: snap.ObservedAt}
		if err != nil {
			state.err = err.Error()
			state.category = string(quota.Category(err))
			if fetchErr == "" {
				fetchErr = err.Error()
			}
		}
		p.polls[id] = state
		p.mu.Unlock()
	}
	if ctx.Err() != nil {
		// The credential list was read only in part. Publishing it would drop
		// rows the plugin is still routing on, so the previous poll's view
		// stands and refresh reports the cut-short context.
		return cfg.Quota.PollInterval
	}
	p.quota.Prune(keep)

	p.mu.Lock()
	p.listErr = ""
	p.fetchErr = fetchErr
	p.auths = governed
	for id := range p.polls {
		if _, ok := keep[id]; !ok {
			delete(p.polls, id)
		}
	}
	// Cache counters follow the host's listing rather than the fetch set, so a
	// credential the plugin serves but does not poll keeps its status row.
	for id := range p.cache {
		if _, ok := listed[id]; !ok {
			delete(p.cache, id)
		}
	}
	p.mu.Unlock()
	return cfg.Quota.PollInterval
}

// warnSingleCandidate logs once per provider that spreading cannot work: the
// host offers only the highest priority tier, so a pool whose credentials do
// not share one priority value collapses to a single candidate. The pick that
// observes the condition only records it, because a host call on the pick path
// has no timeout and would park the request's goroutine for good.
func (p *Plugin) warnSingleCandidate() {
	p.mu.Lock()
	pending := make([]string, 0, len(p.singleCandidates))
	for provider, count := range p.singleCandidates {
		if count == 1 && !p.singleLogged[provider] {
			p.singleLogged[provider] = true
			pending = append(pending, provider)
		}
	}
	p.mu.Unlock()

	sort.Strings(pending)
	for _, provider := range pending {
		p.host.log("warn", "cpa-claude-quota-scheduler: a single candidate was offered; spreading cannot work", map[string]any{
			"provider": provider,
			"hint":     "every credential in the pool must share one priority value",
		})
	}
}

// fetchOne reads one credential's access token from the host and fetches its
// usage. The credential JSON carries refresh tokens as well; only access_token
// is decoded, and neither the payload nor the token is ever logged. A
// credential without an access token is not OAuth and yields a zero snapshot.
func (p *Plugin) fetchOne(ctx context.Context, client *quota.Client, id string, entry HostAuthFileEntry) (model.AuthSnapshot, error) {
	label := authLabel(entry)
	auth, err := p.host.authGet(ctx, entry.AuthIndex)
	if err != nil {
		return model.AuthSnapshot{
			AuthID:      id,
			AuthIndex:   entry.AuthIndex,
			Label:       label,
			ObservedAt:  p.now(),
			Source:      model.SourceUsageEndpoint,
			Err:         "credential unavailable from host",
			ErrCategory: string(quota.CategoryTransport),
		}, err
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(auth.JSON, &token) != nil || token.AccessToken == "" {
		return model.AuthSnapshot{}, nil
	}
	snap, err := client.Fetch(ctx, id, entry.AuthIndex, token.AccessToken)
	snap.Label = label
	// Staleness is judged against the plugin's clock, so the observation is
	// stamped with it rather than with the client's.
	snap.ObservedAt = p.now()
	if err != nil {
		p.host.log("warn", "cpa-claude-quota-scheduler usage fetch failed", map[string]any{
			"auth_id":  id,
			"category": string(quota.Category(err)),
			"error":    err.Error(),
		})
	}
	return snap, err
}

// authID is the identifier that matches SchedulerAuthCandidate.ID and
// UsageRecord.AuthID. The host's disk fallback listing carries no id, only a
// file name, which is the same value for file-backed credentials.
func authID(entry HostAuthFileEntry) string {
	if entry.ID != "" {
		return entry.ID
	}
	return entry.Name
}

// authLabel is the operator-facing name for a credential: the host label,
// else the account email, else the file name. None of these is a secret.
func authLabel(entry HostAuthFileEntry) string {
	switch {
	case entry.Label != "":
		return entry.Label
	case entry.Email != "":
		return entry.Email
	default:
		return entry.Name
	}
}
