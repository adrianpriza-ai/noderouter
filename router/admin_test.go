package router

// The admin guard's fail-closed contract (F4): an empty expected token
// admits nobody. That state is only reachable when the startup token
// generation failed and no auth.adminToken is configured - before the fix,
// an absent X-Noderouter-Token header compared equal to "" and every
// /noderouter/* route was served with no credential at all.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"noderouter/config"
	"noderouter/logx"
	"noderouter/proxy"
)

func adminTestServer() *Server {
	cfg := &config.Config{}
	cfg.ApplyDefaults()
	return NewServer(cfg, proxy.NewPool("", "", "rotate", false), logx.New(false))
}

// With the dynamic token empty and no auth.adminToken set, every admin
// route answers 401 - including a request that presents no credential,
// which is the exact case the fail-open bug let through.
func TestAdminFailsClosedWhenDynamicTokenEmpty(t *testing.T) {
	srv := adminTestServer()
	srv.dynamicAdminToken = "" // simulate the logged generation failure

	cases := []struct{ method, path string }{
		{"GET", "/noderouter/status"},
		{"GET", "/noderouter/routes"},
		{"POST", "/noderouter/refresh"},
		{"POST", "/noderouter/reload"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential = %d, want 401", c.method, c.path, rec.Code)
		}
	}
}

// Fail closed means nobody gets in: even a valid client api-key must not
// open the admin routes while the expected admin token is empty.
func TestAdminFailsClosedForApiKeyHolderWhenDynamicTokenEmpty(t *testing.T) {
	srv := adminTestServer()
	srv.dynamicAdminToken = ""
	srv.cfg.Auth.Token = "test-key"

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/noderouter/status", nil)
	r.Header.Set("X-Noderouter-Token", "test-key")
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status with a valid api-key but empty admin token = %d, want 401", rec.Code)
	}
}

// The startup-generated token still works as before.
func TestAdminDynamicTokenWorks(t *testing.T) {
	srv := adminTestServer()
	if srv.dynamicAdminToken == "" {
		t.Fatal("expected a generated dynamic admin token")
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/noderouter/status", nil)
	r.Header.Set("X-Noderouter-Token", srv.dynamicAdminToken)
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with the dynamic token", rec.Code)
	}
}

// With auth.adminToken configured, the fixed token gates the admin routes
// exactly as before the fail-closed guard was added.
func TestAdminConfiguredTokenStillWorks(t *testing.T) {
	srv := adminTestServer()
	srv.cfg.Auth.AdminToken = "fixed-admin-token"

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/noderouter/routes", nil)
	r.Header.Set("X-Noderouter-Token", "fixed-admin-token")
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("routes = %d, want 200 with the configured admin token", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/noderouter/routes", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("routes with no token = %d, want 401", rec.Code)
	}
}

// A configured adminToken does not disturb the api-key pool's access to
// the admin routes: any client key still opens them.
func TestAdminApiKeyPoolAccessUnchanged(t *testing.T) {
	srv := adminTestServer()
	srv.cfg.Auth.AdminToken = "fixed-admin-token"
	srv.cfg.Auth.Token = "client-key"

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/noderouter/status", nil)
	r.Header.Set("Authorization", "Bearer client-key")
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for an api-key holder", rec.Code)
	}
}

// The admin guard accepts its token in exactly two spellings - the raw
// X-Noderouter-Token header and "Authorization: Bearer <token>" - and
// refuses everything else: a wrong token in either spelling, the right
// token without the Bearer prefix, or no credential at all. This pins the
// validation chain so a future edit cannot quietly widen it.
func TestAdminTokenAcceptedOnlyInItsTwoSpellings(t *testing.T) {
	srv := adminTestServer()
	srv.cfg.Auth.AdminToken = "fixed-admin-token"

	cases := []struct {
		name   string
		header func(r *http.Request)
		want   int
	}{
		{"raw header", func(r *http.Request) {
			r.Header.Set("X-Noderouter-Token", "fixed-admin-token")
		}, http.StatusOK},
		{"bearer header", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer fixed-admin-token")
		}, http.StatusOK},
		{"wrong raw token", func(r *http.Request) {
			r.Header.Set("X-Noderouter-Token", "not-the-token")
		}, http.StatusUnauthorized},
		{"wrong bearer token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer not-the-token")
		}, http.StatusUnauthorized},
		{"right token without the Bearer prefix", func(r *http.Request) {
			r.Header.Set("Authorization", "fixed-admin-token")
		}, http.StatusUnauthorized},
		{"no credential", func(r *http.Request) {}, http.StatusUnauthorized},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/noderouter/status", nil)
		c.header(r)
		srv.Handler().ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

// The api-key pool reaches the admin routes in any of the three header
// spellings a forwarded request accepts, and a key that is not in the pool
// is still refused even with a configured adminToken in place.
func TestAdminApiKeySpellingsAllAccepted(t *testing.T) {
	srv := adminTestServer()
	srv.cfg.Auth.AdminToken = "fixed-admin-token"
	srv.cfg.Auth.Token = "client-key"

	accept := []struct {
		name   string
		header func(r *http.Request)
	}{
		{"Authorization: Bearer", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer client-key")
		},
		},
		{"x-api-key", func(r *http.Request) {
			r.Header.Set("X-Api-Key", "client-key")
		},
		},
		{"X-Noderouter-Token", func(r *http.Request) {
			r.Header.Set("X-Noderouter-Token", "client-key")
		},
		},
	}
	for _, c := range accept {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/noderouter/status", nil)
		c.header(r)
		srv.Handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Errorf("api key via %s: status = %d, want 200", c.name, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/noderouter/status", nil)
	r.Header.Set("Authorization", "Bearer not-in-the-pool")
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown key: status = %d, want 401", rec.Code)
	}
}
