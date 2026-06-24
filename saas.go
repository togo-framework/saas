// Package saas turns a togo app into a multi-tenant SaaS. It resolves the current
// tenant from each request — by domain/subdomain or a tenant-id/team header — and
// scopes the app to it, with two isolation strategies: a shared database scoped by
// tenant_id (row-level), or a single database per tenant (connection routing).
//
// Enable by blank-import: `togo install togo-framework/saas`.
//
// Config (togo.yaml / env):
//
//	SAAS_TENANT_RESOLVER = header | domain | subdomain   (default: header; header = X-Tenant-ID)
//	SAAS_ISOLATION       = shared | single-db            (default: shared)
//
// In a handler: read the tenant with saas.CurrentTenant(ctx) / saas.TenantID(ctx),
// and get the right *sql.DB with the saas service's DB(ctx) (the kernel DB under
// "shared", or the tenant's own DB under "single-db").
package saas

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/togo-framework/orm"
	"github.com/togo-framework/togo"
)

type ctxKey struct{}

func init() {
	// Priority 1: the tenant middleware must register BEFORE any route-mounting
	// plugin (auth/billing/…) — chi forbids Use() after routes exist. `log` runs at
	// priority 0 and mounts no routes, so 1 is the earliest route-touching slot.
	togo.RegisterProviderFunc("saas", togo.PriorityCore+1, func(k *togo.Kernel) error {
		s := &Service{
			k:         k,
			resolver:  resolverFromEnv(),
			isolation: isolationFromEnv(),
			conns:     map[string]*sql.DB{},
		}
		if err := s.migrate(context.Background()); err != nil {
			return err
		}
		k.Router.Use(s.Middleware)
		s.routes()
		k.Set("saas", s)
		return nil
	})
}

// Tenant is an isolated customer of the app.
type Tenant struct {
	ID        string `db:"id" json:"id"`
	Name      string `db:"name" json:"name"`
	Domain    string `db:"domain" json:"domain"`
	Plan      string `db:"plan" json:"plan"`
	DBDSN     string `db:"db_dsn" json:"db_dsn"`
	CreatedAt string `db:"created_at" json:"created_at"`
}

// Service is the saas service, stored in the kernel container under "saas".
type Service struct {
	k         *togo.Kernel
	resolver  Resolver
	isolation string
	mu        sync.Mutex
	conns     map[string]*sql.DB // per-tenant connections (single-db isolation)
}

// From fetches the saas service from the kernel container.
func From(k *togo.Kernel) (*Service, bool) {
	v, ok := k.Get("saas")
	if !ok {
		return nil, false
	}
	s, ok := v.(*Service)
	return s, ok
}

func (s *Service) sysDB(ctx context.Context) (*sql.DB, error) { return s.k.SQL(ctx) }
func (s *Service) tenants(db *sql.DB) *orm.Query[Tenant] {
	return orm.For[Tenant](db, s.k.Dialect(), "tenants")
}

func (s *Service) migrate(ctx context.Context) error {
	db, err := s.sysDB(ctx)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS tenants (id text PRIMARY KEY, name text NOT NULL DEFAULT '', domain text NOT NULL DEFAULT '', plan text NOT NULL DEFAULT '', db_dsn text NOT NULL DEFAULT '', created_at text NOT NULL)`)
	return err
}

// ── Tenant resolution ─────────────────────────────────────────────────────────

// Resolver extracts a tenant key (a domain or a tenant id) from a request.
type Resolver func(r *http.Request) string

func hostOnly(r *http.Request) string {
	host := r.Host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}

var (
	resolversMu sync.RWMutex
	resolvers   = map[string]Resolver{
		// tenant-id / team: an explicit header (set from a JWT claim, a path, etc.).
		"header": func(r *http.Request) string { return r.Header.Get("X-Tenant-ID") },
		// domain-as-tenant: the full host maps to Tenant.Domain.
		"domain": hostOnly,
		// subdomain-as-tenant: the first label (acme.app.com → "acme").
		"subdomain": func(r *http.Request) string {
			h := hostOnly(r)
			if i := strings.IndexByte(h, '.'); i >= 0 {
				return h[:i]
			}
			return ""
		},
	}
)

// RegisterResolver adds (or overrides) a named tenant resolver. Call from init().
func RegisterResolver(name string, r Resolver) {
	resolversMu.Lock()
	defer resolversMu.Unlock()
	resolvers[name] = r
}

func resolverFromEnv() Resolver {
	name := os.Getenv("SAAS_TENANT_RESOLVER")
	if name == "" {
		name = "header"
	}
	resolversMu.RLock()
	defer resolversMu.RUnlock()
	if r, ok := resolvers[name]; ok {
		return r
	}
	return resolvers["header"]
}

func isolationFromEnv() string {
	switch strings.ToLower(os.Getenv("SAAS_ISOLATION")) {
	case "single-db", "single", "database", "db-per-tenant":
		return "single-db"
	default:
		return "shared"
	}
}

// ── Middleware ──────────────────────────────────────────────────────────────

// Middleware resolves the current tenant from the request and stores it in the
// context (read it with CurrentTenant). Registered on the kernel router.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.resolver != nil {
			if key := s.resolver(r); key != "" {
				if t, ok := s.lookup(r.Context(), key); ok {
					r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, t))
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// lookup finds a tenant by domain, then by id.
func (s *Service) lookup(ctx context.Context, key string) (*Tenant, bool) {
	db, err := s.sysDB(ctx)
	if err != nil {
		return nil, false
	}
	if t, err := s.tenants(db).Where("domain", "=", key).First(ctx); err == nil && t != nil {
		return t, true
	}
	if t, err := s.tenants(db).Where("id", "=", key).First(ctx); err == nil && t != nil {
		return t, true
	}
	return nil, false
}

// CurrentTenant returns the tenant resolved for this request, if any.
func CurrentTenant(ctx context.Context) (*Tenant, bool) {
	t, ok := ctx.Value(ctxKey{}).(*Tenant)
	return t, ok
}

// TenantID returns the current tenant id ("" if none) — use it to scope queries
// under the shared-database isolation strategy.
func TenantID(ctx context.Context) string {
	if t, ok := CurrentTenant(ctx); ok {
		return t.ID
	}
	return ""
}

// DB returns the database to use for the current request: under "single-db"
// isolation it is the tenant's own database (opened from Tenant.DBDSN, cached);
// otherwise the shared kernel database — scope your queries by TenantID(ctx).
func (s *Service) DB(ctx context.Context) (*sql.DB, error) {
	if s.isolation == "single-db" {
		if t, ok := CurrentTenant(ctx); ok && t.DBDSN != "" {
			return s.tenantDB(t)
		}
	}
	return s.k.SQL(ctx)
}

func (s *Service) tenantDB(t *Tenant) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db, ok := s.conns[t.ID]; ok {
		return db, nil
	}
	db, err := sql.Open(s.k.Config.DBDriver, t.DBDSN)
	if err != nil {
		return nil, err
	}
	s.conns[t.ID] = db
	return db, nil
}

// ── Tenant CRUD API ───────────────────────────────────────────────────────────

func (s *Service) routes() {
	r := s.k.Router
	base := strings.TrimRight(s.k.Config.RESTPath, "/") + "/tenants"
	r.Get(base, s.list)
	r.Post(base, s.create)
	r.Get(base+"/{id}", s.get)
	r.Put(base+"/{id}", s.update)
	r.Delete(base+"/{id}", s.del)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	db, err := s.sysDB(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ts, err := s.tenants(db).Order("created_at DESC").Get(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	var b Tenant
	_ = json.NewDecoder(r.Body).Decode(&b)
	db, err := s.sysDB(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rec, err := s.tenants(db).Create(r.Context(), map[string]any{
		"id":         newID(),
		"name":       b.Name,
		"domain":     b.Domain,
		"plan":       b.Plan,
		"db_dsn":     b.DBDSN,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	db, err := s.sysDB(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	t, err := s.tenants(db).Where("id", "=", chi.URLParam(r, "id")).First(r.Context())
	if err != nil || t == nil {
		writeErr(w, http.StatusNotFound, "tenant not found")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Service) update(w http.ResponseWriter, r *http.Request) {
	var b map[string]any
	_ = json.NewDecoder(r.Body).Decode(&b)
	delete(b, "id")
	delete(b, "created_at")
	db, err := s.sysDB(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.tenants(db).Where("id", "=", chi.URLParam(r, "id")).Update(r.Context(), b); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Service) del(w http.ResponseWriter, r *http.Request) {
	db, err := s.sysDB(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.tenants(db).Where("id", "=", chi.URLParam(r, "id")).Delete(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
