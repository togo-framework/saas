<p align="center"><img src="https://to-go.dev/togo-mark.svg" width="120" alt="togo"/></p>
<h1 align="center">togo · saas</h1>
<p align="center">Multi-tenant SaaS for togo — turn any app or dashboard into a multi-tenant product.</p>

---

`saas` resolves the **current tenant** from each request and scopes the app to it, with pluggable **tenant resolution** and **isolation** strategies — so you can model tenants as **domains**, as **teams / tenant-ids**, and store their data in **one shared database** or **a database per tenant**.

## Install

```bash
togo install togo-framework/saas
```

Blank-importing the package registers the plugin; it creates a `tenants` table and adds the tenant middleware + the tenant CRUD API on boot.

## Config (`togo.yaml` / env)

```yaml
# togo.yaml
saas:
  tenant_resolver: header   # header | domain | subdomain
  isolation: shared         # shared | single-db
```

| env | values | default | meaning |
|---|---|---|---|
| `SAAS_TENANT_RESOLVER` | `header` · `domain` · `subdomain` | `header` | how the tenant is identified per request |
| `SAAS_ISOLATION` | `shared` · `single-db` | `shared` | where each tenant's data lives |

## Tenant resolution (who is this request for?)

- **`header`** — a **tenant-id / team** in the `X-Tenant-ID` header (set it from a JWT claim, a path segment, your gateway, …). Best for teams/orgs inside one app.
- **`domain`** — **domain-as-tenant**: the full host (`acme.com`) maps to `Tenant.Domain`.
- **`subdomain`** — the first label (`acme.app.com` → `acme`).

Register your own: `saas.RegisterResolver("path", func(r *http.Request) string { … })` and set `SAAS_TENANT_RESOLVER=path`.

## Isolation (where does the data live?)

- **`shared`** — one database, every tenant row carries a `tenant_id`. Scope your queries with `saas.TenantID(ctx)`. Cheapest; good default.
- **`single-db`** — **one database per tenant**: each tenant row stores a `db_dsn`, and `Service.DB(ctx)` returns that tenant's own connection (cached). Strongest isolation.

## Use it in a handler

```go
import "github.com/togo-framework/saas"

func handler(w http.ResponseWriter, r *http.Request) {
    t, ok := saas.CurrentTenant(r.Context())     // the resolved tenant
    if !ok { http.Error(w, "no tenant", 400); return }

    s, _ := saas.From(kernel)
    db, _ := s.DB(r.Context())                    // tenant DB (single-db) or shared DB
    // shared isolation: filter by saas.TenantID(r.Context())
    _ = db
}
```

## Tenant CRUD API

```
GET    /api/tenants          list tenants
POST   /api/tenants          create { name, domain, plan, db_dsn }
GET    /api/tenants/{id}     get
PUT    /api/tenants/{id}     update
DELETE /api/tenants/{id}     delete
```

## License

MIT
