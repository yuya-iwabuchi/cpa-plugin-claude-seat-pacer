package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/quota"
	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/session"
)

// Options configures a Plugin. Name, Version, Author and Repository are the
// registration metadata the host refuses to load without.
type Options struct {
	Name         string
	Version      string
	Author       string
	Repository   string
	ConfigFields []ConfigField

	// Host performs host callbacks. Nil leaves every callback failing, which
	// is how the plugin behaves after cliproxyPluginShutdown.
	Host HostFunc
	// NewBindingStore builds the affinity table. Nil selects session.NewStore;
	// tests supply a fake.
	NewBindingStore NewBindingStore
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Plugin owns every piece of runtime state and answers every host method.
//
// Concurrency: the host calls scheduler.pick, request.intercept_before and
// usage.handle concurrently with each other and with lifecycle methods and
// management requests. Config is an atomic pointer swapped whole; the quota
// store, binding store and decision log lock internally; everything else
// mutable sits behind mu, held only for field access and never across a
// host callback.
type Plugin struct {
	opts Options
	host host
	now  func() time.Time

	cfg       atomic.Pointer[model.Config]
	quota     *quota.Store
	decisions *decisionLog
	resource  atomic.Pointer[resourceHandler]

	// handle dispatches one method; tests replace it to exercise the guard.
	handle func(method string, payload []byte) ([]byte, error)

	mu          sync.Mutex
	bindings    BindingStore
	bindingsTTL time.Duration
	bindingsMax int
	startedAt   time.Time
	hostSchema  uint32
	lastModel   string
	cache       map[string]model.CacheStats
	auths       []HostAuthFileEntry
	polls       map[string]pollState
	listErr     string
	fetchErr    string
	// singleCandidates is the candidate count each provider last offered a
	// cold pick, and singleLogged the providers already warned about.
	singleCandidates map[string]int
	singleLogged     map[string]bool
	mgmtBase         string
	resourceBase     string

	lifeMu sync.Mutex
	poller *poller
	pollMu sync.Mutex
	// startDelay is the wait before the first poll; tests push it out so a
	// poll cannot race their assertions.
	startDelay time.Duration
}

// resourceHandler wraps an http.Handler so it fits an atomic.Pointer.
type resourceHandler struct {
	h http.Handler
}

// pollState is the last poll outcome for one credential, for the status view.
type pollState struct {
	at       time.Time
	err      string
	category string
}

// New returns an unregistered plugin. Nothing runs until plugin.register
// arrives; until then Config is the defaults with Enabled false.
func New(opts Options) *Plugin {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewBindingStore == nil {
		opts.NewBindingStore = func(ttl time.Duration, maxSessions int) BindingStore {
			return session.NewStore(ttl, maxSessions)
		}
	}
	p := &Plugin{
		opts:             opts,
		host:             newHost(opts.Host),
		now:              opts.Now,
		quota:            quota.NewStore(),
		cache:            make(map[string]model.CacheStats),
		polls:            make(map[string]pollState),
		singleCandidates: make(map[string]int),
		singleLogged:     make(map[string]bool),
		startDelay:       startupGrace,
	}
	initial := model.Defaults()
	initial.Enabled = false
	p.cfg.Store(&initial)
	p.decisions = newDecisionLog(initial.Web.HistoryLimit)
	p.handle = p.dispatch
	return p
}

// SetResourceHandler installs the status app served on the plugin's resource
// routes. The host serves those routes unauthenticated, so the handler must
// expose read-only, credential-free content.
func (p *Plugin) SetResourceHandler(h http.Handler) {
	if h == nil {
		p.resource.Store(nil)
		return
	}
	p.resource.Store(&resourceHandler{h: h})
}

// config returns the live configuration.
func (p *Plugin) config() model.Config {
	return *p.cfg.Load()
}

// Call answers one host method with a response envelope. ok is false when the
// envelope is an error. It never panics: recover() does not cross the C
// boundary, and one escaped panic fuses the plugin for every capability it
// declares, so the guard sits here and again in the cgo shim.
func (p *Plugin) Call(method string, payload []byte) (raw []byte, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			raw, ok = degrade(method, codePluginPanic, fmt.Sprintf("recovered: %v", r))
			p.host.log("error", "cpa-claude-quota-scheduler recovered from a panic", map[string]any{
				"method": method,
				"panic":  fmt.Sprintf("%v", r),
			})
		}
	}()
	raw, err := p.handle(method, payload)
	if err != nil {
		return degrade(method, codePluginError, err.Error())
	}
	var env Envelope
	if json.Unmarshal(raw, &env) == nil && !env.OK {
		return raw, false
	}
	return raw, true
}

// degrade is the answer for a method the plugin could not serve. It is per
// method because an error envelope costs more than a decline on every hook the
// host routes traffic through: scheduler.pick hard-fails the request with no
// fallback to the host's own selector, the interceptors fail it downstream,
// and management.handle turns into a 502. Only the lifecycle methods, where an
// error is the honest answer and the host handles it, keep one.
func degrade(method, code, message string) ([]byte, bool) {
	var result any
	switch method {
	case MethodSchedulerPick:
		result = SchedulerPickResponse{Handled: false}
	case MethodRequestInterceptBefore:
		// A failed interceptor still owes the pick a clean slate: without the
		// clear a client's own bridge headers reach it.
		result = clearBridge()
	case MethodRequestInterceptAfter, MethodUsageHandle:
		result = emptyResult
	case MethodManagementHandle:
		// The message may name plugin internals and the resource routes are
		// unauthenticated, so the body says only that the call failed.
		result = jsonResponse(http.StatusInternalServerError, map[string]string{"error": "plugin request failed"})
	default:
		return errorEnvelope(code, message), false
	}
	raw, err := okEnvelope(result)
	if err != nil {
		return errorEnvelope(code, message), false
	}
	return raw, true
}

func (p *Plugin) dispatch(method string, payload []byte) ([]byte, error) {
	switch method {
	case MethodPluginRegister, MethodPluginReconfigure:
		return p.configure(payload)
	case MethodPluginQuiesce, MethodPluginShutdown:
		p.Shutdown()
		return okEnvelope(emptyResult)
	case MethodRequestInterceptBefore:
		return p.interceptBefore(payload)
	case MethodRequestInterceptAfter:
		return okEnvelope(emptyResult)
	case MethodSchedulerPick:
		return p.pickEnvelope(payload)
	case MethodUsageHandle:
		return p.usageEnvelope(payload)
	case MethodManagementRegister:
		return p.managementRegister(payload)
	case MethodManagementHandle:
		return p.managementHandle(payload)
	default:
		return errorEnvelope(codeUnknownMethod, "unknown method: "+method), nil
	}
}

// configure answers plugin.register and plugin.reconfigure. Both carry the
// same payload and the host re-issues reconfigure on every config reload and
// several times during startup, so the work here is a config swap and a
// no-op poller start.
func (p *Plugin) configure(payload []byte) ([]byte, error) {
	var req LifecycleRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			// An undecodable request is answered like an empty config block:
			// the plugin registers inert rather than failing to load.
			p.host.log("warn", "cpa-claude-quota-scheduler could not decode the lifecycle request", map[string]any{"error": err.Error()})
			req = LifecycleRequest{}
		}
	}
	cfg, err := decodeConfig(req.ConfigYAML)
	if err != nil {
		p.host.log("warn", "cpa-claude-quota-scheduler config is invalid; plugin is inert", map[string]any{"error": err.Error()})
	}
	p.applyConfig(cfg)

	p.mu.Lock()
	p.hostSchema = req.SchemaVersion
	if p.startedAt.IsZero() {
		p.startedAt = p.now()
	}
	p.mu.Unlock()

	p.startPoller()
	return okEnvelope(RegisterResult{
		SchemaVersion: SchemaVersion,
		Metadata:      p.metadata(),
		Capabilities: map[string]bool{
			CapabilityRequestInterceptor: true,
			CapabilityScheduler:          true,
			CapabilityUsagePlugin:        true,
			CapabilityManagementAPI:      true,
		},
	})
}

func (p *Plugin) metadata() Metadata {
	fields := p.opts.ConfigFields
	if fields == nil {
		fields = []ConfigField{}
	}
	return Metadata{
		Name:             p.opts.Name,
		Version:          p.opts.Version,
		Author:           p.opts.Author,
		GitHubRepository: p.opts.Repository,
		ConfigFields:     fields,
	}
}

// applyConfig publishes a config and resizes the state that depends on it.
// The binding table is rebuilt only when its TTL or cap changes, because a
// rebuild discards every live binding and costs each conversation a cache
// miss.
func (p *Plugin) applyConfig(cfg model.Config) {
	p.cfg.Store(&cfg)
	p.decisions.resize(cfg.Web.HistoryLimit)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bindings == nil || p.bindingsTTL != cfg.Affinity.TTL || p.bindingsMax != cfg.Affinity.MaxSessions {
		p.bindings = p.opts.NewBindingStore(cfg.Affinity.TTL, cfg.Affinity.MaxSessions)
		p.bindingsTTL = cfg.Affinity.TTL
		p.bindingsMax = cfg.Affinity.MaxSessions
	}
}

// bindingStore returns the live binding table.
func (p *Plugin) bindingStore() BindingStore {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bindings == nil {
		cfg := p.cfg.Load()
		p.bindings = p.opts.NewBindingStore(cfg.Affinity.TTL, cfg.Affinity.MaxSessions)
		p.bindingsTTL = cfg.Affinity.TTL
		p.bindingsMax = cfg.Affinity.MaxSessions
	}
	return p.bindings
}

// record appends a decision and remembers the model it routed, which is the
// model the status view defaults to.
func (p *Plugin) record(d model.Decision) {
	p.decisions.add(d)
	if d.Kind != model.DecisionDeclined && d.Model != "" {
		p.mu.Lock()
		p.lastModel = d.Model
		p.mu.Unlock()
	}
}

// headerValue looks a header up case-insensitively, so a map that arrives
// non-canonical still hits.
func headerValue(headers map[string][]string, name string) string {
	if values, ok := headers[name]; ok && len(values) > 0 {
		return values[0]
	}
	canonical := http.CanonicalHeaderKey(name)
	for key, values := range headers {
		if len(values) > 0 && http.CanonicalHeaderKey(key) == canonical {
			return values[0]
		}
	}
	return ""
}

// metadataString reads a string-valued metadata key, tolerating absence and
// other types.
func metadataString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	value, _ := meta[key].(string)
	return value
}
