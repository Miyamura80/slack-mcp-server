package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

const testAPIKey = "test-key"

// fakeFactory records how many tenants it built and which token each request
// reached, echoing the token-derived tenant back so tests can see routing.
type fakeFactory struct {
	built     atomic.Int32
	reject    map[string]bool
	sawHeader atomic.Bool
}

func (f *fakeFactory) build(token string) (http.Handler, error) {
	if f.reject[token] {
		return nil, errors.New("invalid_auth")
	}
	f.built.Add(1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(TenantTokenHeader) != "" {
			f.sawHeader.Store(true)
		}
		_, _ = w.Write([]byte(token))
	}), nil
}

func newTestRouter(f *fakeFactory, max int) *TenantRouter {
	return NewTenantRouter(testAPIKey, max, f.build, zap.NewNop())
}

func doRequest(tr *TenantRouter, path, apiKey, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if token != "" {
		req.Header.Set(TenantTokenHeader, token)
	}
	rec := httptest.NewRecorder()
	tr.ServeHTTP(rec, req)
	return rec
}

func TestUnitTenantRouter_RejectsBadAPIKeyBeforeBuildingTenant(t *testing.T) {
	f := &fakeFactory{}
	tr := newTestRouter(f, 10)

	assert.Equal(t, http.StatusUnauthorized, doRequest(tr, "/mcp", "", "xoxb-a").Code)
	assert.Equal(t, http.StatusUnauthorized, doRequest(tr, "/mcp", "wrong", "xoxb-a").Code)
	assert.Equal(t, int32(0), f.built.Load())
	assert.Equal(t, 0, tr.Len())
}

func TestUnitTenantRouter_RequiresTokenHeader(t *testing.T) {
	tr := newTestRouter(&fakeFactory{}, 10)
	rec := doRequest(tr, "/mcp", testAPIKey, "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), TenantTokenHeader)
}

func TestUnitTenantRouter_OnlyServesMCPPath(t *testing.T) {
	tr := newTestRouter(&fakeFactory{}, 10)
	assert.Equal(t, http.StatusNotFound, doRequest(tr, "/other", testAPIKey, "xoxb-a").Code)
}

func TestUnitTenantRouter_RoutesByTokenAndReusesTenants(t *testing.T) {
	f := &fakeFactory{}
	tr := newTestRouter(f, 10)

	assert.Equal(t, "xoxb-a", doRequest(tr, "/mcp", testAPIKey, "xoxb-a").Body.String())
	assert.Equal(t, "xoxb-b", doRequest(tr, "/mcp", testAPIKey, "xoxb-b").Body.String())
	assert.Equal(t, "xoxb-a", doRequest(tr, "/mcp", testAPIKey, "xoxb-a").Body.String())

	assert.Equal(t, int32(2), f.built.Load())
	assert.Equal(t, 2, tr.Len())
	assert.False(t, f.sawHeader.Load(), "token header must be stripped before the tenant handler")
}

func TestUnitTenantRouter_BuildsEachTenantOnceUnderConcurrency(t *testing.T) {
	f := &fakeFactory{}
	tr := newTestRouter(f, 10)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doRequest(tr, "/mcp", testAPIKey, "xoxb-a")
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), f.built.Load())
}

func TestUnitTenantRouter_EvictsLeastRecentlyUsed(t *testing.T) {
	f := &fakeFactory{}
	tr := newTestRouter(f, 2)

	doRequest(tr, "/mcp", testAPIKey, "xoxb-a")
	doRequest(tr, "/mcp", testAPIKey, "xoxb-b")
	doRequest(tr, "/mcp", testAPIKey, "xoxb-a") // a is now most recent
	doRequest(tr, "/mcp", testAPIKey, "xoxb-c") // evicts b

	assert.Equal(t, 2, tr.Len())
	assert.Equal(t, int32(3), f.built.Load())

	doRequest(tr, "/mcp", testAPIKey, "xoxb-a") // still held
	assert.Equal(t, int32(3), f.built.Load())
	doRequest(tr, "/mcp", testAPIKey, "xoxb-b") // rebuilt
	assert.Equal(t, int32(4), f.built.Load())
}

func TestUnitTenantRouter_RejectedTokenIsNotCached(t *testing.T) {
	f := &fakeFactory{reject: map[string]bool{"xoxb-bad": true}}
	tr := newTestRouter(f, 10)

	rec := doRequest(tr, "/mcp", testAPIKey, "xoxb-bad")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotContains(t, rec.Body.String(), "xoxb-bad", "the token must never be echoed back")
	assert.Equal(t, 0, tr.Len())

	// Once Slack accepts it (e.g. the app was reinstalled), a retry succeeds.
	delete(f.reject, "xoxb-bad")
	assert.Equal(t, http.StatusOK, doRequest(tr, "/mcp", testAPIKey, "xoxb-bad").Code)
}
