package server

import (
	"container/list"
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"go.uber.org/zap"
)

// TenantTokenHeader carries the caller's Slack bot token in multi-tenant mode.
const TenantTokenHeader = "X-Slack-Bot-Token"

// TenantFactory builds the MCP HTTP handler serving one bot token.
type TenantFactory func(token string) (http.Handler, error)

type tenantEntry struct {
	key     string
	once    sync.Once
	handler http.Handler
	err     error
	elem    *list.Element
}

// TenantRouter serves many Slack bots from one HTTP endpoint. Each request
// names its bot in TenantTokenHeader; the router keeps one MCP server per
// token (created on first use, least recently used evicted past maxTenants)
// and hands the request to it. Routing is deterministic by token, so a
// client's Streamable HTTP session always lands on the server that owns it.
type TenantRouter struct {
	apiKey     string
	maxTenants int
	newTenant  TenantFactory
	logger     *zap.Logger

	mu      sync.Mutex
	tenants map[string]*tenantEntry
	lru     *list.List
}

func NewTenantRouter(apiKey string, maxTenants int, newTenant TenantFactory, logger *zap.Logger) *TenantRouter {
	if maxTenants < 1 {
		maxTenants = 1
	}
	return &TenantRouter{
		apiKey:     apiKey,
		maxTenants: maxTenants,
		newTenant:  newTenant,
		logger:     logger,
		tenants:    make(map[string]*tenantEntry),
		lru:        list.New(),
	}
}

func (tr *TenantRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}

	// Check the shared API key before touching the token, so unauthenticated
	// callers can never make the server call Slack or allocate a tenant.
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(bearer), []byte(tr.apiKey)) != 1 {
		http.Error(w, "invalid or missing API key", http.StatusUnauthorized)
		return
	}

	token := strings.TrimSpace(r.Header.Get(TenantTokenHeader))
	if token == "" {
		http.Error(w, "missing "+TenantTokenHeader+" header", http.StatusUnauthorized)
		return
	}
	// The token is only needed to pick the tenant; don't pass it further.
	r.Header.Del(TenantTokenHeader)

	entry := tr.acquire(provider.TokenKey(token))
	entry.once.Do(func() {
		entry.handler, entry.err = tr.newTenant(token)
	})
	if entry.err != nil {
		tr.drop(entry)
		tr.logger.Warn("Rejected Slack bot token",
			zap.String("context", "http"),
			zap.String("tenant", entry.key[:12]),
			zap.Error(entry.err),
		)
		http.Error(w, "Slack rejected the bot token in "+TenantTokenHeader, http.StatusUnauthorized)
		return
	}

	entry.handler.ServeHTTP(w, r)
}

// acquire returns the entry for key, creating it if needed and marking it most
// recently used. Creation of the tenant itself happens outside the lock.
func (tr *TenantRouter) acquire(key string) *tenantEntry {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	if e, ok := tr.tenants[key]; ok {
		tr.lru.MoveToFront(e.elem)
		return e
	}

	e := &tenantEntry{key: key}
	e.elem = tr.lru.PushFront(e)
	tr.tenants[key] = e

	for tr.lru.Len() > tr.maxTenants {
		oldest := tr.lru.Back()
		evicted := oldest.Value.(*tenantEntry)
		tr.lru.Remove(oldest)
		delete(tr.tenants, evicted.key)
		tr.logger.Info("Evicted idle Slack tenant",
			zap.String("context", "http"),
			zap.String("tenant", evicted.key[:12]),
		)
	}
	return e
}

// drop forgets a tenant whose creation failed so a later request can retry.
func (tr *TenantRouter) drop(e *tenantEntry) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if cur, ok := tr.tenants[e.key]; ok && cur == e {
		tr.lru.Remove(e.elem)
		delete(tr.tenants, e.key)
	}
}

// Len reports how many tenants are currently held.
func (tr *TenantRouter) Len() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.lru.Len()
}

// NewBotTenantFactory returns the production TenantFactory: it authenticates
// the token against Slack, builds a provider and MCP server for it, and warms
// the users/channels caches, waiting up to readyWait before serving so the
// first call can resolve #channel and @user names.
func NewBotTenantFactory(enabledTools []string, noCache bool, readyWait time.Duration, logger *zap.Logger) TenantFactory {
	return func(token string) (http.Handler, error) {
		tenantLogger := logger.With(zap.String("tenant", provider.TokenKey(token)[:12]))

		p, err := provider.NewForBotToken("http", token, tenantLogger)
		if err != nil {
			return nil, err
		}
		s := NewMCPServer(p, tenantLogger, enabledTools)

		if noCache {
			p.SkipCache()
		} else {
			go warmTenantCaches(p, tenantLogger)
			deadline := time.Now().Add(readyWait)
			for time.Now().Before(deadline) {
				if ready, _ := p.IsReady(); ready {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}

		tenantLogger.Info("Slack tenant ready", zap.String("context", "http"))
		return s.ServeHTTP(""), nil
	}
}

// warmTenantCaches fills a tenant's caches. Errors are logged, not fatal: in
// multi-tenant mode one bot's failure must not stop the others. A failed
// refresh falls back to SkipCache so the tenant still serves ID-based calls
// instead of answering "cache not ready" forever.
func warmTenantCaches(p *provider.ApiProvider, logger *zap.Logger) {
	if err := p.RefreshUsers(context.Background()); err != nil {
		logger.Error("Caching users failed", zap.String("context", "http"), zap.Error(err))
		p.SkipCache()
		return
	}
	if err := p.RefreshChannels(context.Background()); err != nil {
		logger.Error("Caching channels failed", zap.String("context", "http"), zap.Error(err))
		p.SkipCache()
	}
}
