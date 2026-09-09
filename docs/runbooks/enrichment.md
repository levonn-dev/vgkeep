# Enrichment service

The enrichment service is the catalog and pricing quarantine for all
third-party data: IGDB game metadata, PriceCharting prices, and
frankfurter.dev exchange rates enter the system here and nowhere else.
It owns the product catalog (find-or-create identity for games,
hardware and price-anchor listings), scores auto-matches between IGDB
games and PriceCharting listings, keeps a daily price-snapshot series
per product, and runs the moderation surface for community-minted
products. Everything else in the stack (bff, collection) reads
products and prices from it by id.

Component and flow diagrams, the data model, and the env contract
live in [services/enrichment/README.md](../../services/enrichment/README.md).

Features as an operator sees them:

- Catalog search (`GET /search`, kinds game / hardware / pc_listing),
  Valkey-cached 24h, with admin-minted community products interleaved
  into game and hardware results. Game results are physical-only:
  digital-only game types are excluded at IGDB and the digital-only
  platforms in `api/domain.yaml` are pruned. Provider down: answers
  degrade to a local Postgres name match, flagged `degraded: true` and
  never cached.
- Product resolve (`POST /products/resolve`): find-or-create keyed by
  provider identity. No-pick game resolves run the auto-matcher
  against PriceCharting listings, taking the entry region as a
  matching input: the console gate admits only the base, "pal
  "-prefixed, or JP-named console axis for the region class, and an
  ntsc_j resolve queries by the ja-JP transliteration when the catalog
  carries one, with one fallback search by the alternate (canonical
  name) form when the gate admits nothing from it. Nothing at or above
  score 0.75 (`match.Threshold`) means the product lands unmatched
  rather than guessed.
- Product reads (`GET /products/{productId}`), Valkey-cached 5m, with
  inline best-effort refetch of IGDB projections older than
  `IGDB_REFRESH_AFTER` (720h default).
- Batch prices and price history (`POST /products/prices:batch`,
  `POST /products/price-history:batch`, up to 500 ids each), read
  straight from Postgres. The collection service is the main caller.
- Recommendation scoring (`POST /recommendations:score`): user-agnostic
  scoring over the shared `igdb_raw` metadata cache, library up to
  2500 entries, candidate budget 200.
- FX rates (`GET /fx/latest`) and the platform catalog
  (`GET /platforms`, minus the digital-only platforms).
- The catalog refresh (`POST /internal/refresh`, CronJob at 06:00,
  guarded by a service token minted from auth's
  `/internal/service-token`): price refresh + snapshot for every
  mapped product, projection rebuild for every IGDB-bearing product
  (refetching only raws behind the current `fields_version`), and the
  promote-candidate sweep over community products. 30 minute budget,
  one refresh at a time (a concurrent trigger answers 409).
- Normalize community regions (`POST
  /internal/normalize-community-regions`, admin role or a service
  token; the enrichment-refresh CronJob runs it right after the
  refresh trigger): promotes free-text community-product regions into
  the known set, exact-or-synonym folding only, never fuzzy.
- Admin moderation (JWT role `admin`): mapping fix, community product
  mint / promote / delete, unmatched and community worklists,
  promote-candidate review and dismiss, immediate refresh trigger.
  Worklist paging (`limit`, 1-500, default 200) rejects an
  out-of-range value with 400 `invalid_param` instead of clamping it
  into bounds; operators see the same code-plus-detail error body
  every other validation failure already answers with.

## Architecture

```mermaid
graph LR
    subgraph vgkeep callers
        bff[bff]
        coll[collection]
        cron["enrichment-refresh CronJob 06:00"]
    end
    subgraph enrichment pod
        mig["init: enrichment migrate"] --> svc["enrichment :8080"]
    end
    bff -- "JWT (user + admin routes)" --> svc
    coll -- "JWT (prices, resolve)" --> svc
    cron -.->|"exchange for a service token"| auth
    cron -- "JWT (service token)" --> svc
    svc -- "TLS verify-full :5432" --> pg[(enrichment-pg)]
    svc -- "TLS, fail-open cache" --> valkey[("enrichment-valkey :6379")]
    svc -- "JWKS fetch" --> auth["auth :8080"]
    svc -- "4 req/s" --> igdb["IGDB api.igdb.com"]
    svc -- "app token" --> twitch["id.twitch.tv"]
    svc -- "1 req/s" --> pc["pricecharting.com"]
    svc --> fx["api.frankfurter.dev"]
    es[ExternalSecret enrichment-secrets] -.-> svc
    svc -- "OTLP 4317" --> otel[otel-agent]
```

The NetworkPolicy `enrichment-from-callers-only` admits exactly bff,
collection and the enrichment-refresh job pods on 8080; the APISIX
gateway (8090) publishes only the bff and never routes here. The
datastore policies admit only the enrichment pod plus the vg-platform
Prometheus (exporter sidecars on 9187 postgres, 9121 valkey).

The catalog refresh is the one flow where the HTTP answer and the work
are decoupled, which trips people up during triage. The CronJob is an
exchange-then-trigger: it authenticates once with auth's static
internal secret, then presents the minted service token to enrichment
like any other Bearer caller:

```mermaid
sequenceDiagram
    participant J as refresh Job (curl)
    participant A as auth
    participant E as enrichment
    participant P as PriceCharting
    participant D as enrichment-pg
    participant V as enrichment-valkey
    J->>A: POST /internal/service-token (X-Internal-Token)
    A-->>J: 200 service JWT (900s, token_use=service)
    J->>E: POST /internal/refresh (Bearer service JWT)
    E-->>J: 202 started (refresh detaches, 30m budget)
    Note over J: Job success means "trigger accepted", nothing more
    E->>D: ListPriced
    loop every mapped product
        E->>P: current prices (1 req/s)
        E->>D: update current + append snapshot
        E->>V: invalidate product key
    end
    Note over E: then reprojection, candidate sweep
    E->>E: log "price refresh finished" processed/updated/failures
```

## Running it

Dev stack is Tilt (`task run`). The Tilt resource `enrichment` depends
on `secret-store`, `enrichment-pg`, `enrichment-valkey` and `auth`;
`enrichment-refresh` depends on `enrichment`. Image builds from
`services/enrichment/Dockerfile` with only `libs/go` and
`services/enrichment` in context, so edits elsewhere do not roll it.

| Where       | What                                                                                                |
| ----------- | --------------------------------------------------------------------------------------------------- |
| Service     | localhost:8084 -> pod 8080 (Tilt port-forward)                                                      |
| enrichment-pg | localhost:5437 -> pod 5432                                                                        |
| Valkey      | no port-forward (TLS-only, in-cluster callers)                                                      |
| Gateway     | not published; call 8084 directly with a JWT                                                        |
| Bruno flows | `bruno/enrichment/` (search, resolve, prices, history, recommendations, admin refresh, admin remap, normalize community regions) |

Task targets: root `task lint`, `task build`, `task test:short`,
`task test:cover` (80 percent per-module gate via
`scripts/coverage.sh`), `task check` before committing. Module-scoped
in `services/enrichment/`: `task gen` regenerates
`internal/gen/api/server.gen.go` from `api/enrichment.yaml`, and
`task db:migrate` runs `go run ./cmd/enrichment migrate` against
`DATABASE_URL` (also runs under root `task migrate`, alongside every
other migrate-capable service).

Health endpoints, outside JWT auth:

- `GET /healthz` answers 200 whenever the process is up.
- `GET /readyz` pings Postgres via pgkit.Health and nothing else.
  Postgres is the hard dependency; Valkey is deliberately absent
  because every cache call fails open. A Postgres outage therefore
  takes the pod out of Service endpoints after the probe's failure
  threshold.

`POST /internal/refresh` sits behind the same blanket JWT middleware
as every other route (no more standing shared secret at this service):
its own `requireService` check then requires the claim
`token_use=service`, so a plain user's own access token is forbidden
and only a service token minted by auth's `/internal/service-token`
passes. The NetworkPolicy stays the outer layer.

Migrate mode: `enrichment migrate` loads the full config, runs the
embedded migrations via pgkit.Migrate and exits. One migration to
date (`000001_schema`, embedded in `services/enrichment/migrations/`).
The deployment runs it as an init container with the same env anchor
as the app, so every rollout migrates before serving.

Startup order matters at boot only: main connects and pings Postgres,
then Valkey, and exits on either failure (crash loop with backoff
until both answer). At runtime Postgres stays hard (readyz) and Valkey
soft (per-request fail-open).

## Configuration

All env is declared in `services/enrichment/internal/config/config.go`
and wired by `deploy/charts/enrichment/templates/deployment.yaml`.
Secret material flows .env -> Tilt's `vg-fake` ClusterSecretStore ->
ExternalSecret `enrichment-secrets` (refreshInterval 1m) -> pod env.

| Env var                       | Value / source                                                                                                                                                                  |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `HTTP_ADDR`                   | code default `:8080` (chart does not set it)                                                                                                                                    |
| `DATABASE_URL`                | chart-composed: `postgres://enrichment:$(PG_PASSWORD)@enrichment-pg:5432/enrichment?sslmode=verify-full&sslrootcert=/etc/vg/pg-ca/ca.crt`                                       |
| `PG_PASSWORD`                 | secret `enrichment-pg-credentials` key `password`, filled by the ExternalSecret from ClusterSecretStore `vg-fake` key `enrichment/pg-password` (.env `PG_ENRICHMENT_PASSWORD`)  |
| `VALKEY_URL`                  | chart `env.valkeyUrl` = `rediss://enrichment-valkey:6379/0`                                                                                                                     |
| `VALKEY_CA_FILE`              | `/etc/vg/valkey-ca/ca.crt` when `valkey.enabled`; config refuses a `rediss://` URL without it                                                                                   |
| `JWKS_URL`                    | `http://auth:8080/.well-known/jwks.json`                                                                                                                                        |
| `JWT_ISSUER` / `JWT_AUDIENCE` | `vgkeep-auth` / `vgkeep`                                                                                                                                                        |
| `IGDB_MODE`                   | `stub` (default) or `real`; real requires `IGDB_CLIENT_ID` + `IGDB_CLIENT_SECRET` from secret keys                                                                              |
| `PRICECHARTING_MODE`          | `stub` or `real`; real requires `PRICECHARTING_API_KEY`                                                                                                                         |
| `FX_MODE`                     | chart default `real` (frankfurter.dev is credential-less); code default `stub`                                                                                                  |
| `SEARCH_CACHE_TTL`            | `24h` (search + platform catalog cache)                                                                                                                                         |
| `PRODUCT_CACHE_TTL`           | `5m`                                                                                                                                                                            |
| `IGDB_REFRESH_AFTER`          | `720h` (IGDB projection and platform-catalog staleness horizon)                                                                                                                 |
| `SERVICE_VERSION`             | image tag, stamped on telemetry as service.version                                                                                                                              |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://otel-agent.vg-platform.svc.cluster.local:4317`; empty string disables export (JSON logs to stdout only)                                                                 |

Absent optional pieces: stub providers serve embedded fixtures, so a
credential-less checkout runs the whole feature set deterministically.
Tilt flips `igdb.mode` / `pricecharting.mode` to real only when the
full credential set is present in .env; flipping by hand on a partial
set points the ExternalSecret at store keys that were never published
and wedges the secret sync. Config validation also refuses real modes
without their credentials. This service holds no shared secret of its
own anymore: every caller, human or machine, authenticates with a JWT
(see auth.md for `INTERNAL_SERVICE_SECRETS`, the CronJob-facing
secret's new home).

## Datastores

Postgres (`enrichment-pg`, StatefulSet, postgres:17-alpine, 1Gi PVC)
holds the catalog schema. `products` is one row per catalog product;
identity is enforced by unique partial indexes: `products_game_identity`
on (igdb_game_id, platform_igdb_id, pc_product_id) `NULLS NOT DISTINCT`
scoped to `type = 'game' AND origin = 'provider'`,
`products_hardware_identity` on (pc_product_id, region, edition,
variant) `NULLS NOT DISTINCT` scoped to `type IN ('console',
'accessory') AND origin = 'provider'`, and `products_pc_listing_identity`
on (pc_product_id) scoped to `type = 'pc_listing'` (unscoped by origin -
a community product can never be type pc_listing); the
identity-bearing columns (`igdb_game_id`, `platform_igdb_id`,
`pc_product_id`) are `GENERATED ALWAYS AS ... STORED` projections off
the `igdb`/`platform`/`pricecharting` jsonb columns, so the indexes
stay plain btrees over generated scalars rather than expression
indexes over jsonb paths. A plain `products_name` index backs the
degraded local search, and `products_unmatched_worklist` on
(updated_at, id) filtered to `origin = 'provider' AND pricecharting IS
NULL` covers the admin unmatched worklist's filter+sort. Community
products (`origin = 'community'`) sit outside the origin-scoped
identity indexes on purpose: their identity is the curated name, and
the promote flow re-enters them through the index.
`igdb_raw` is the shared raw-payload cache (recommendations and
reprojection read it; every provider fetch populates it backwards).
`platforms` caches the IGDB platform catalog wholesale.
`price_snapshots` is range-partitioned on `captured_at`: one partition
per year (2026 through 2036 today) plus a `price_snapshots_default`
catch-all, primary key (product_id, captured_at), and
`product_id REFERENCES products(id) ON DELETE CASCADE` - append-only in
practice, and snapshots survive product mapping changes by design. One
migration to date (`000001_schema`); the down file exists and is
exercised by the migrate tooling, not by hand.

Connection facts: TLS verify-full against the in-cluster CA (secret
`enrichment-pg-tls`), same shape as every other Postgres-backed
service in this stack. The postgres-exporter sidecar serves 9187,
scraped via ServiceMonitor every 30s (`service` label `enrichment-pg`).

Valkey (`enrichment-valkey`, StatefulSet, valkey:8-alpine) is a pure
cache: TLS-only listener on 6379, no client cert auth, no persistence
(`--save ""`, `--appendonly no`, emptyDir), so a restart starts cold
and everything rebuilds from providers and Postgres. Keys:
`search:v4:<kind>:<sha256(query)>` (24h), `product:v1:<uuid>` (5m),
`platforms:v1` (24h). The redis_exporter sidecar serves 9121
(`service` label `enrichment-valkey`).

Pool metrics: the pgkit and valkeykit clients each register the shared
pool instruments, scoped to this service by the `service_name`
resource attribute:

| OTel name                            | Prometheus name                            | Answers                                                |
| ------------------------------------ | ------------------------------------------ | ------------------------------------------------------ |
| `vg.pgkit.pool.connections`          | `vg_pgkit_pool_connections`                | open connections                                       |
| `vg.pgkit.pool.connections.idle`     | `vg_pgkit_pool_connections_idle`           | idle headroom                                          |
| `vg.pgkit.pool.connections.max`      | `vg_pgkit_pool_connections_max`            | configured pool ceiling                                |
| `vg.pgkit.pool.acquires`             | `vg_pgkit_pool_acquires_total`             | cumulative successful checkouts                        |
| `vg.pgkit.pool.empty_acquires`       | `vg_pgkit_pool_empty_acquires_total`       | checkouts that found no idle connection waiting        |
| `vg.pgkit.pool.acquire_wait`         | `vg_pgkit_pool_acquire_wait_seconds_total` | cumulative time callers spent waiting for a connection |
| `vg.valkeykit.pool.hits`             | `vg_valkeykit_pool_hits_total`             | acquires served by a free connection                   |
| `vg.valkeykit.pool.misses`           | `vg_valkeykit_pool_misses_total`           | acquires that dialed a new connection                  |
| `vg.valkeykit.pool.timeouts`         | `vg_valkeykit_pool_timeouts_total`         | callers that gave up waiting (hard saturation)         |
| `vg.valkeykit.pool.connections`      | `vg_valkeykit_pool_connections`            | open connections                                       |
| `vg.valkeykit.pool.connections.idle` | `vg_valkeykit_pool_connections_idle`       | idle headroom                                          |

Server-side `pg_stat_activity_count{service="enrichment-pg"}` from the
exporter covers the same connection counting from the server's point
of view.

## Telemetry

The pipeline is libs/go/otel `Setup()`: OTLP traces, metrics and logs
to otel-agent -> otel-gateway -> Prometheus (remote write, exemplars
on), Loki and Jaeger, plus slog JSON on stdout with trace ids
attached. Traces already cover the full request path: otelhttp server
spans (route-labeled), otelpgx client spans per query, redisotel
command spans, and otelhttp client spans for IGDB, PriceCharting,
frankfurter and the Twitch token endpoint.

Emitted today (Prometheus-side names):

| Metric                                                                                                                                       | Source                       | Labels                                                                                  | Answers                  |
| -------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------- | --------------------------------------------------------------------------------------- | ------------------------ |
| `http_server_request_duration_seconds_{count,sum,bucket}`                                                                                    | otelhttp middleware          | `http_route`, `http_response_status_code` (+ `service_name="enrichment"` resource attr) | RED for every route      |
| `go_goroutine_count`, `go_memory_used_bytes`                                                                                                 | otel runtime instrumentation | `service_name`                                                                          | runtime health           |
| `vg_pgkit_pool_*` (table above)                                                                                                              | pgkit                        | none                                                                                    | client pool health       |
| `vg_valkeykit_pool_*` (table above)                                                                                                          | valkeykit                    | none                                                                                    | client pool health       |
| `pg_stat_activity_count`, `pg_settings_max_connections`, `pg_stat_database_xact_commit`, `pg_stat_database_xact_rollback`                    | postgres-exporter sidecar    | `service="enrichment-pg"`                                                               | server-side Postgres health |
| `redis_memory_used_bytes`, `redis_keyspace_hits_total`, `redis_keyspace_misses_total`, `redis_evicted_keys_total`, `redis_connected_clients` | redis exporter sidecar       | `service="enrichment-valkey"`                                                           | server-side cache health |

Domain instruments, meter
`github.com/levonn-dev/vgkeep/services/enrichment`, held as
`Handlers` fields with best-effort registration (the bff
`vg.bff.cache.fail_open` pattern):

| Metric                                  | Instrument             | Unit        | Labels (bounded)                                                                                                                                                                                                              | Prometheus name                                                  | Question it answers                                                                                                                                                                                                            |
| --------------------------------------- | ---------------------- | ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `vg.enrichment.cache.fail_open`         | Int64Counter           | `{event}`   | `op`: search_get, search_decode, search_put, product_get, product_put, platforms_get, platforms_put, community_search, refresh_invalidate, reprojection_invalidate, mapping_invalidate, promote_invalidate, delete_invalidate | `vg_enrichment_cache_fail_open_total`                            | is Valkey failing and which operation absorbs it                                                                                                                                                                               |
| `vg.enrichment.search.requests`         | Int64Counter           | `{request}` | `kind`: game, hardware, pc_listing; `source`: cache, provider, degraded                                                                                                                                                       | `vg_enrichment_search_requests_total`                            | search cache effectiveness per kind, and the user-visible degraded share (provider outage)                                                                                                                                     |
| `vg.enrichment.search.localization_leg` | Int64Counter           | `{leg}`     | `outcome`: merged, empty, error                                                                                                                                                                                               | `vg_enrichment_search_localization_leg_total`                    | is the non-Latin supplementary search leg running, and how often it merges extra results, finds nothing extra, or fails (error still serves primary results - never an outage)                                                 |
| `vg.enrichment.match.outcomes`          | Int64Counter           | `{attempt}` | `source`: resolve; `outcome`: matched, below_threshold, provider_down; `region`: ntsc_u, ntsc_j, pal, region_free, korea, brazil, china, none                                                                                                       | `vg_enrichment_match_outcomes_total`                             | is the auto-matcher landing matches at its usual rate, or regressing into unmatched members, broken out by region                                                                                                              |
| `vg.enrichment.match.fallback_search`   | Int64Counter           | `{search}`  | `outcome`: matched, still_empty, error                                                                                                                                                                                        | `vg_enrichment_match_fallback_search_total`                      | is the fallback name-form search (fired only when the primary query's results miss the platform/region gate and a second name form exists) finding a match, coming up empty, or erroring                                       |
| `vg.enrichment.refresh.items`           | Int64Counter           | `{item}`    | `step`: prices, reprojection, sweep; `outcome`: ok, failed, skipped, flagged                                                                                                                                                  | `vg_enrichment_refresh_items_total`                              | how much of the catalog the catalog refresh processed and what share failed                                                                                                                                                    |
| `vg.enrichment.refresh.step_duration`   | Float64Histogram       | `s`         | `step`: prices, reprojection, sweep                                                                                                                                                                                           | `vg_enrichment_refresh_step_duration_seconds_{count,sum,bucket}` | did each catalog refresh step run today, and how close the refresh is to its 30m budget                                                                                                                                        |
| `vg.enrichment.refresh.last_completed`  | Float64ObservableGauge | `s`         | `step`: prices, reprojection, sweep (present only once that step has completed in this process)                                                                                                                               | `vg_enrichment_refresh_last_completed_seconds`                   | when did each catalog refresh step last actually complete - the restart-proof signal the stalled-refresh alert reads, since a gauge keeps re-reporting its last value across a pod replacement instead of resetting to nothing |
| `vg.enrichment.normalize.regions`       | Int64Counter           | `{row}`     | `outcome`: normalized, skipped, failed                                                                                                                                                                                        | `vg_enrichment_normalize_regions_total`                          | is the nightly community-region promotion sweep landing: how many free-text community-product region rows get promoted vs left as typed (no known value or synonym match) vs fail on the store write, each run                 |

Emission sites, split across `internal/server/`:

- `cache.fail_open`: one increment inside `Handlers.failOpen`
  (`internal/server/server.go`), which every fail-open call site
  already routes through; `op` is the string those call sites already
  pass.
- `search.requests`: one increment per answered `SearchCatalog`
  request (`internal/server/handlers_search.go`) at the point the
  source is known (cache hit, provider answer, or degraded local
  fallback). The resolve-side `searchPCListingsCached` helper does not
  count; this metric means "user-facing search answers".
- `search.localization_leg`: one increment per `searchGames` call
  (`internal/server/handlers_search.go`) whose query trips
  `hasNonLatinLetter`; a Latin query never reaches this counter.
  `outcome` is `merged` when the `game_localizations` where-filter leg
  (`SearchLocalizations`) answers and its ids are folded into
  consideration (whether or not any were new), `empty` when it
  answers with nothing, `error` when the leg call or its follow-up
  `GamesByIDs` fetch for newly found ids fails. Only `error` is worth
  a look, and even then the primary IGDB search results already
  served as-is - the leg is a best-effort widening, never a hard
  dependency, so this counter is a feature-health signal, not an
  availability one.
- `match.outcomes`: in `autoMatchGame`
  (`internal/server/handlers_products.go`), with the caller passing
  the source (`resolveGame` no-pick path = resolve - the only calling
  flow today) and the clamped entry region as the `region` label.
  provider_down maps to the existing "auto-match skipped" warn,
  below_threshold to the existing info line.
- `match.fallback_search`: also in `autoMatchGame`
  (`internal/server/handlers_products.go`), fires only when the
  primary query's platform/region-filtered results come up empty and
  a second name form exists (nothing to fall back to never trips it).
  `error` when the fallback search call itself fails; otherwise the
  leg merges into the same match pass, and the outcome is `matched`
  when that then clears the threshold, `still_empty` when it still
  does not.
- `refresh.items`: incremented alongside the per-item tallies each
  step's loop already keeps (`internal/server/handlers_admin.go`). Per
  step, `outcome` means: prices ok = price written and snapshot
  appended, failed = fetch/write/snapshot failure; reprojection ok =
  projection rewritten, skipped = diff-gate unchanged or unusable raw;
  sweep ok = swept clean, flagged = candidates stashed, failed =
  provider or store failure.
- `refresh.step_duration`: recorded once per step
  (`internal/server/handlers_admin.go`) next to that step's own
  completion log line ("price refresh finished" for prices,
  "reprojection finished" for reprojection, "candidate sweep
  complete" for sweep - and on early abort, so a stopped step still
  reports its elapsed time). Explicit bucket boundaries 1, 5, 15, 60,
  300, 900, 1800 seconds; the defaults top out at 10s and would
  flatten every step into one bucket.
- `refresh.last_completed`: stamped in the same call as
  `refresh.step_duration` above (`recordRefreshStepDuration`,
  `internal/server/server.go`), into a per-step unix-time map an
  Observable gauge callback reads at export time. A step this process
  has not completed yet is simply absent from the callback's
  observations, never a false zero - the property that keeps the
  gauge restart-proof, since Prometheus still holds whatever value the
  prior process last exported.

Log additions (slog, JSON, trace ids attached):

| Event                                  | Level | Fields                             | Emission site                                                                                               |
| -------------------------------------- | ----- | ---------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `catalog refresh started`              | INFO  | `trigger` = admin or internal      | `startRefresh`, immediately after winning the in-flight guard                                                |
| `normalize-community-regions complete` | INFO  | `scanned`, `normalized`, `skipped` | `InternalNormalizeCommunityRegions`, before writing the response                                             |
| `handler error`                          | ERROR | `op`, `err`                        | `internalError`, the shared 500 helper every handler file's error branches now route through (29 call sites) |

The refresh-started line pairs with the existing per-step "finished"
summaries: a started line without finished lines inside the 30m
budget is the signature of a hung refresh. Everything else the
refresh needs is already logged (finished summaries with counts,
stopped-early warns, per-product failure warns, panic containment).
`handler error` is the general line every route's 500 now emits (see
failure mode 1 below): `op` names the failing operation, the same
labeling idiom `cache.fail_open`'s `op` attribute already uses, so a
500 traces to its cause without cross-referencing which route or
collaborator was in play.

Region coverage for localized titles is a second, independent
mechanism from the search leg above: `BundleLocalizations`
(`internal/igdb/igdb.go`) is what actually builds each region's
bundle - both the product's `igdb.localizations` payload and a
search result's `matched_region` read it - by merging a game's
`game_localizations` rows (authoritative name and cover, no table
needed; every region IGDB returns is read) with alternative names
mined from `alternative_names` via the `altTagFamilies` table: a
per-region prefix/exclude rule (today `ja-JP` prefix "japanese
title", `ko-KR` prefix "korean title", `zh-CN` prefix "simplified
chinese title", `zh-TW` prefix "traditional chinese title", `pt-BR`
prefix "portuguese title", all excluding comments containing
"translat" so an English retranslation cannot steal the native-name
slot). A region whose native title only shows up in
`alternative_names` - not in `game_localizations` - needs a row in
this table; that is the extension point for the next region. New
families take effect at the next reprojection (nightly refresh or
POST /admin/refresh) - no raw refetch, alternative_names are already
in every raw. The full region checklist, this table included, is
docs/adding-a-region.md.

The pricing half now consumes these same bundles for a different
purpose: `matchNamesFor` reads the `ja-JP` chain's transliteration as
an ntsc_j resolve's primary PriceCharting query form (PriceCharting
files JP listings in romaji), falling back to the canonical name when
the region carries no bundle. Entries matched before this landed still
point at their old, region-incompatible member; collection's entry
rematch (`POST /internal/rematch-entries`, see
[collection.md](collection.md)) is the entry-side sweep that repoints
them.

Search-result platform refs carry a separate signal, `release_regions`
(`platformReleaseRegions`, `internal/server/handlers_search.go`): per-platform
canonical release-date regions, platform-exact and ordered
earliest-first, independent of BundleLocalizations above - the field
the frontend's availability badges key off.

## Dashboard: vg-enrichment

File `deploy/charts/platform/files/dashboards/enrichment.json`, uid
`vg-enrichment`, title `Enrichment Service`, provisioned into the
vgkeep folder like every dashboard in that directory. Open it at
http://localhost:3000/d/vg-enrichment while `task run` holds the
Grafana port-forward. It follows the structural conventions shared by
every vgkeep dashboard: schemaVersion 39, tags `["vgkeep"]`,
timezone browser, refresh 30s, explicit datasource object per target
(prometheus, loki). Panels, grouped by row:

Overview:

1. "Availability" - timeseries, short, legend `{{pod}}`

    ```promql
    up{namespace="vgkeep", pod=~"enrichment-.*"}
    ```

2. "Request rate" - stat, reqps

    ```promql
    sum(rate(http_server_request_duration_seconds_count{service_name="enrichment"}[5m]))
    ```

3. "5xx ratio" - stat, percentunit; state thresholds green under 0.05 /
    red at 0.05 (the vg-service-5xx page objective)

    ```promql
    sum(rate(http_server_request_duration_seconds_count{service_name="enrichment",http_response_status_code=~"5.."}[5m])) / sum(rate(http_server_request_duration_seconds_count{service_name="enrichment"}[5m]))
    ```

4. "p99 latency" - stat, s; state thresholds green under 0.5 / yellow
    at 0.5 (the vg-service-p99 warn objective)

    ```promql
    histogram_quantile(0.99, sum by (le) (rate(http_server_request_duration_seconds_bucket{service_name="enrichment"}[5m])))
    ```

HTTP (all Prometheus, scoped `service_name="enrichment"`):

5. "Request rate by route" - timeseries, reqps, legend `{{http_route}}`

    ```promql
    sum by (http_route) (rate(http_server_request_duration_seconds_count{service_name="enrichment"}[$__rate_interval]))
    ```

6. "5xx ratio (5m)" - timeseries, percentunit

    ```promql
    sum(rate(http_server_request_duration_seconds_count{service_name="enrichment",http_response_status_code=~"5.."}[5m])) / sum(rate(http_server_request_duration_seconds_count{service_name="enrichment"}[5m]))
    ```

7. "Latency by route (p95/p99)" - timeseries, s, `"exemplar": true` on
    both targets, legends `p95 {{http_route}}` / `p99 {{http_route}}`

    ```promql
    histogram_quantile(0.95, sum by (le, http_route) (rate(http_server_request_duration_seconds_bucket{service_name="enrichment"}[$__rate_interval])))
    histogram_quantile(0.99, sum by (le, http_route) (rate(http_server_request_duration_seconds_bucket{service_name="enrichment"}[$__rate_interval])))
    ```

8. "4xx and 5xx by route and status" - timeseries, reqps, legend
    `{{http_route}} {{http_response_status_code}}`

    ```promql
    sum by (http_route, http_response_status_code) (rate(http_server_request_duration_seconds_count{service_name="enrichment",http_response_status_code=~"4..|5.."}[$__rate_interval]))
    ```

Search and matching:

9. "Search answers by source" - timeseries, short, legend
    `{{kind}} {{source}}`

    ```promql
    sum by (kind, source) (increase(vg_enrichment_search_requests_total[5m]))
    ```

10. "Auto-match outcomes" - timeseries, short, legend
    `{{source}}/{{outcome}}/{{region}}`

    ```promql
    sum by (source, outcome, region) (increase(vg_enrichment_match_outcomes_total[1h]))
    ```

11. "Match fallback searches" - timeseries, short, legend `{{outcome}}`
    (only fired fallback legs count; most resolves never trip it)

    ```promql
    sum by (outcome) (increase(vg_enrichment_match_fallback_search_total[1h]))
    ```

12. "Localization search legs" - timeseries, short, legend
    `{{outcome}}` (only non-Latin queries reach this leg; `error`
    still serves primary results, so this panel is a feature-health
    signal, not an availability one)

    ```promql
    sum by (outcome) (rate(vg_enrichment_search_localization_leg_total[5m]))
    ```

Catalog refresh and sweeps:

13. "Catalog refresh items by step and outcome" - timeseries, short,
    legend `{{step}} {{outcome}}`

    ```promql
    sum by (step, outcome) (increase(vg_enrichment_refresh_items_total[1h]))
    ```

14. "Catalog refresh duration by step" - timeseries, s, legend `{{step}}`
    (one refresh per day: the 1h increase of the sum is the last refresh's
    elapsed seconds at the refresh hour, zero elsewhere)

    ```promql
    sum by (step) (increase(vg_enrichment_refresh_step_duration_seconds_sum[1h]))
    ```

15. "Time since refresh completed by step" - timeseries, s, legend
    `{{step}}`; the alert threshold draws an orange line at 93600s (26h) -
    see failure mode 4 below

    ```promql
    time() - max by (step) (last_over_time(vg_enrichment_refresh_last_completed_seconds[26h]))
    ```

16. "Normalize sweeps (rows/night by outcome)" - timeseries, short,
    legend `regions/{{outcome}}` (24h window: one increase per night)

    ```promql
    sum by (outcome) (increase(vg_enrichment_normalize_regions_total[24h]))
    ```

PostgreSQL (from this service's seat):

17. "PG pool connections" - timeseries, short, legends `in pool` /
    `idle` / `max`

    ```promql
    vg_pgkit_pool_connections{service_name="enrichment"}
    vg_pgkit_pool_connections_idle{service_name="enrichment"}
    vg_pgkit_pool_connections_max{service_name="enrichment"}
    ```

18. "PG pool mean acquire wait" - timeseries, s

    ```promql
    rate(vg_pgkit_pool_acquire_wait_seconds_total{service_name="enrichment"}[5m]) / rate(vg_pgkit_pool_acquires_total{service_name="enrichment"}[5m])
    ```

19. "PG pool empty acquires" - timeseries, short

    ```promql
    increase(vg_pgkit_pool_empty_acquires_total{service_name="enrichment"}[5m])
    ```

20. "PG server connections vs max" - timeseries, short, legends
    `connections` / `max`

    ```promql
    sum(pg_stat_activity_count{service="enrichment-pg"})
    max(pg_settings_max_connections{service="enrichment-pg"})
    ```

21. "PG transactions" - timeseries, ops, legends `commit` / `rollback`

    ```promql
    sum(rate(pg_stat_database_xact_commit{service="enrichment-pg",datname!~"template.*"}[$__rate_interval]))
    sum(rate(pg_stat_database_xact_rollback{service="enrichment-pg",datname!~"template.*"}[$__rate_interval]))
    ```

Valkey:

22. "Valkey pool connections" - timeseries, short, legends
    `open` / `idle`

    ```promql
    vg_valkeykit_pool_connections{service_name="enrichment"}
    vg_valkeykit_pool_connections_idle{service_name="enrichment"}
    ```

23. "Valkey pool acquire outcomes" - timeseries, ops, legends `hits` /
    `misses` / `timeouts`

    ```promql
    rate(vg_valkeykit_pool_hits_total{service_name="enrichment"}[$__rate_interval])
    rate(vg_valkeykit_pool_misses_total{service_name="enrichment"}[$__rate_interval])
    rate(vg_valkeykit_pool_timeouts_total{service_name="enrichment"}[$__rate_interval])
    ```

24. "Valkey pool reuse ratio" - timeseries, percentunit

    ```promql
    rate(vg_valkeykit_pool_hits_total{service_name="enrichment"}[5m]) / (rate(vg_valkeykit_pool_hits_total{service_name="enrichment"}[5m]) + rate(vg_valkeykit_pool_misses_total{service_name="enrichment"}[5m]))
    ```

25. "Valkey server memory" - timeseries, bytes

    ```promql
    redis_memory_used_bytes{service="enrichment-valkey"}
    ```

26. "Valkey evictions and clients" - timeseries, short, legends
    `evictions` / `clients`

    ```promql
    rate(redis_evicted_keys_total{service="enrichment-valkey"}[$__rate_interval])
    redis_connected_clients{service="enrichment-valkey"}
    ```

27. "Valkey keyspace hit ratio" - timeseries, percentunit

    ```promql
    rate(redis_keyspace_hits_total{service="enrichment-valkey"}[5m]) / (rate(redis_keyspace_hits_total{service="enrichment-valkey"}[5m]) + rate(redis_keyspace_misses_total{service="enrichment-valkey"}[5m]))
    ```

28. "Valkey fail-open events by op" - timeseries, short, legend `{{op}}`

    ```promql
    sum by (op) (increase(vg_enrichment_cache_fail_open_total[5m]))
    ```

Runtime:

29. "Goroutines" - timeseries, short, legend `goroutines`

    ```promql
    go_goroutine_count{service_name="enrichment"}
    ```

30. "Heap used" - timeseries, bytes, legend `heap`

    ```promql
    go_memory_used_bytes{service_name="enrichment"}
    ```

Pods (the `container="enrichment"` selector scopes to the app pod
only; postgres, valkey and the refresh job's own "trigger" container
all carry different container names):

31. "CPU by pod" - timeseries, short, legend `{{pod}}`

    ```promql
    sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="vgkeep", container="enrichment"}[$__rate_interval]))
    ```

32. "Working-set memory by pod" - timeseries, bytes, legend `{{pod}}`

    ```promql
    sum by (pod) (container_memory_working_set_bytes{namespace="vgkeep", container="enrichment"})
    ```

33. "Restarts and OOM kills by pod (15m)" - timeseries, short, legend
    `restarts {{pod}}` / `oom {{pod}}`

    ```promql
    sum by (pod) (increase(kube_pod_container_status_restarts_total{namespace="vgkeep", container="enrichment"}[15m]))
    sum by (pod) (kube_pod_container_status_last_terminated_reason{reason="OOMKilled", namespace="vgkeep", container="enrichment"})
    ```

Logs:

34. "Recent error and warn logs" - logs panel, Loki datasource

    ```logql
    {service_name="enrichment"} | severity_text=~"ERROR|WARN"
    ```

## Failure modes and triage

### 1. 5xx ratio climbing

Confirm on the "5xx ratio" panel or "5xx ratio by service" on
vg-overview; the shared triage in
[stack.md](stack.md#1-service-5xx-ratio-above-5-percent) applies.
Every 500 logs a `handler error` line at ERROR carrying the failing
operation and cause (`op`, `err`) - read it first rather than
inferring the cause from the affected route alone:

```logql
{service_name="enrichment"} |= "handler error"
```

(the "Recent error and warn logs" panel, or the Log additions row under
Telemetry above - `op` names the failing operation; 29 call sites
share 28 distinct op values, one pair deliberately alike.) Enrichment
specifics: a 500 burst on
`GET /products/{productId}` and `POST /products/prices:batch` with
`/readyz` failing means Postgres (failure mode 2); 502s are not
5xx-of-ours in spirit but count in the ratio, and mean a provider
outage (failure mode 3). Latency triage:
[stack.md](stack.md#2-service-p99-latency-above-500ms); use the
exemplars on "Latency by route (p95/p99)" to jump into Jaeger traces.

### 2. Postgres down or saturated

Down: `/readyz` fails (pgkit.Health), the pod goes NotReady, and
enrichment leaves Service endpoints once the readiness probe's failure
threshold trips.

1. Run `kubectl -n vgkeep get pods enrichment-pg-0` to see
   whether the pod is down, crash-looping or just unready.
2. Read the container logs
   (`kubectl -n vgkeep logs enrichment-pg-0 -c postgres`) for the
   startup or crash reason.
3. Read how far enrichment's error rate has climbed on this
   dashboard's "5xx ratio" panel: while Postgres is down, enrichment
   stays graceful only for reads its caches still hold.

The enrichment-side sequence: cache-warm product reads keep answering
up to 5 minutes, search keeps answering from the 24h cache with the
community lane failing open, everything cache-cold 500s, and within
about 30s (three failed 10s readyz probes, kubernetes defaults) the
pod leaves Service endpoints entirely, at which point callers see
connection failures rather than 500s. After Postgres returns the pod
re-readies on its own; no restart needed.

Saturated: [stack.md's Postgres saturation
triage](stack.md#6-postgres-connections-above-80-percent-of-max)
covers the cluster-wide rule (vg-pg-saturation already watches every
pg instance, enrichment-pg included). This service's own "PG pool
connections" and "PG pool mean acquire wait" panels show whether the
client pool itself is the bottleneck; the contention share is

```promql
rate(vg_pgkit_pool_empty_acquires_total{service_name="enrichment"}[5m]) / rate(vg_pgkit_pool_acquires_total{service_name="enrichment"}[5m])
```

### 3. Search degraded

Signature: "Search answers by source" grows a `degraded` series,
users see thinner results with stale prices, and the log line below
appears at WARN. The vg-enrichment-search-degraded rule fires when
the degraded share holds above 10 percent for 15 minutes:

```promql
sum(rate(vg_enrichment_search_requests_total{source="degraded"}[15m])) / sum(rate(vg_enrichment_search_requests_total[15m]))
```

The confirming log line:

```logql
{service_name="enrichment"} |= "search provider unavailable; serving local catalog match"
```

This is the IGDB (kind game) or PriceCharting (hardware, pc_listing)
search path failing. Degraded answers are never cached, so recovery is
automatic on the next request once the provider answers. Check the
provider's own status page and the client spans in Jaeger before
suspecting our side; with `IGDB_MODE`/`PRICECHARTING_MODE` = stub a
degraded answer means the embedded fixtures failed to load, which does
not happen at runtime (stubs are loaded at startup).

Knock-on effects while a provider is down: resolves that need it
answer 502 `upstream_unavailable`, no-pick game resolves land
unmatched (outcome `provider_down` climbing on "Auto-match
outcomes"), and the catalog refresh logs per-product fetch failures. There
is no automated re-match sweep behind it: an unmatched member heals on
the next resolve for its (game, platform, region) triple, or via
collection's entry rematch for entries already pointing at it (see
[collection.md](collection.md)).

### 4. Catalog refresh missing

The catalog refresh is silent-failure-prone: the CronJob can fail
without anyone noticing, and prices just quietly age (visible to users
as stale `pricecharting.as_of` stamps). Confirm: "Catalog refresh
items by step and outcome" and "Catalog refresh duration by step" flat
for more than 24h, or

```promql
time() - max(last_over_time(vg_enrichment_refresh_last_completed_seconds{step="prices"}[26h]))
```

reads above 93600 (26 hours). The signal is a gauge stamped with the
unix time the prices step last completed in this process, read back
through `last_over_time`: unlike the step_duration counter's
increase(), which sees nothing until the current process's own first
measurement, a gauge keeps re-reporting its last known value on every
export interval, so Prometheus already holds a real sample from before
a pod replacement to fall back on - a refresh that actually ran
survives the deploy that would otherwise make this alert fire by
accident. The vg-enrichment-refresh-stalled rule fires on the same
expression, and treats an absent series as firing too (a refresh that
never ran is the failure case), so a brand-new stack alerts until its
first refresh completes at 06:00 or by manual trigger. The enrichment
dashboard charts this as "Time since refresh completed by step" with
the alert threshold drawn at 93600 seconds. Then:

1. `kubectl -n vgkeep get jobs -l app.kubernetes.io/name=enrichment-refresh`
   and `kubectl -n vgkeep logs job/<latest>` for the curl output.
2. curl exit 22 on the FIRST hop (auth): the CronJob's internal secret
   was rejected by `POST /internal/service-token` (401
   `invalid_internal_token`) - token drift against
   `INTERNAL_SERVICE_SECRETS`, mid-rotation state left half-finished.
   Re-check the rotation steps under auth.md's Admin levers. curl exit
   22 on the SECOND hop (enrichment) with a 403 would mean the minted
   token itself is wrong-shaped (unexpected - the exchange endpoint
   always mints token_use=service); a 401 there means the hop DID run
   but with a missing or garbled bearer - the sed extraction drifting
   against the auth response shape, or the short-lived token expiring
   between the mint and this call. No output from the second hop at
   all means it never ran: `-f` makes the first curl exit nonzero on
   its own failure without printing a body, and `&&` short-circuits
   the rest of the line before the second curl fires - check exit
   codes per hop in the job log. A third hop now follows the refresh
   trigger (`/internal/normalize-community-regions`); its own `-f`
   failure fails the job (and counts against backoffLimit) even when
   the refresh step above completed cleanly, so a failed job next to a
   healthy "Catalog refresh items by step and outcome" panel points at
   this third hop, not the refresh - the job log's last curl line
   shows which one actually failed.
3. 409 `refresh_in_progress`: the in-process guard believes a refresh
   is running. The refresh checks its context between products and the
   budget cancels it at 30m, so a 409 persisting well past 30m means
   the refresh goroutine is stuck in a call that ignores its context;
   `kubectl -n vgkeep rollout restart deployment/enrichment`
   clears it (the guard dies with the process).
4. No job at all: CronJob suspended or schedule drift;
   `kubectl -n vgkeep get cronjob enrichment-refresh`.

Once fixed, trigger immediately rather than waiting for 06:00 (see
Admin levers).

### 5. Catalog refresh failing or stopping early

"Catalog refresh items by step and outcome" shows a `failed` share, or
the stopped-early warn appears:

```logql
{service_name="enrichment"} |= "price refresh stopped early: context done"
```

A failed share tracks provider or Postgres trouble mid-refresh; individual
products are skipped, the refresh finishes what it can, and the next
night retries, so occasional failures are self-healing noise. A
stopped-early warn means the 30m budget expired: PriceCharting's 1
req/s ceiling caps a full price refresh at roughly 1800 mapped products
per run, so a catalog past that size stops early every night and
starves the later steps (reprojection, sweep run on what remains).
That is a capacity signal, not an incident: raise the refresh budget or
split the schedule in the service code.

### 6. Valkey failing open

"Valkey fail-open events by op" (`vg_enrichment_cache_fail_open_total` by
op) is the service-side signal; every op degrades to a cache miss, so
the symptom is latency and provider load, never errors. Server-side
memory/eviction triage is
[stack.md](stack.md#7-valkey-evicting-keys-or-memory-unusually-high). A
nonzero `timeouts` series on the "Valkey pool acquire outcomes" panel
means callers waited for the pool and gave up - a saturated pool or a
wedged Valkey. A Valkey restart empties the
cache (no persistence): expect a cold-start burst of provider calls
bounded by the client limiters, and product reads hitting Postgres
until the 5m cache refills. Boot-time exception: the service requires
Valkey at startup, so enrichment pods crash-loop if Valkey is absent
during a deploy.

### 7. 401s across every route

All API routes behind JWT start answering 401 at once: the validator
cannot fetch or verify against `JWKS_URL` (auth service down or its
keys rotated unexpectedly). "4xx and 5xx by route and status" shows 401
across routes simultaneously, which distinguishes this from a single
misbehaving caller. Check the auth pod, then
`curl -s http://localhost:8082/.well-known/jwks.json` via the auth
port-forward. Unlike before the service-token migration,
`/internal/refresh` is NOT immune to an auth outage anymore: the
CronJob's first hop mints its service token from auth, so a down auth
service stalls the catalog refresh too (curl fails at the exchange
step, never reaching enrichment) - one more reason failure mode 4's
"no job at all" / curl-exit-22 triage starts here.

### 8. identity_taken on admin writes

Mapping fixes, clears and promotes can answer 409 `identity_taken`
with the holding product's id and name in the detail. This is the
unique identity index adjudicating, not a fault: two products cannot
carry the same provider identity, and merging is deliberately manual.
Look up the named holder, decide which product survives, and use
delete (unmatched residue) or a different mapping. No telemetry
action.

## Admin levers

All idempotent and safe to re-run; the refresh triggers answer 409
while one runs. Admin JWT: log in as an admin user via the SPA, or in dev
`task grant-fixture-admin` grants the dev fixture the role, and the
Bruno flows (`bruno/enrichment/admin-refresh.bru`, `admin-remap.bru`,
`normalize-community-regions.bru`) script the calls. Normalize
community regions alone also accepts a service token in place of the
admin JWT - the nightly CronJob runs it that way, right after the
refresh trigger.

Run the catalog refresh now, CronJob path (exchanges its own service
token, in-cluster):

```bash
kubectl -n vgkeep create job --from=cronjob/enrichment-refresh refresh-now
```

Same refresh via the admin API (port-forward 8084):

```bash
curl -X POST -H "Authorization: Bearer $ADMIN_JWT" http://localhost:8084/admin/refresh
```

Both run the same steps: prices, reprojection, candidate sweep. The
reprojection step is the catalog's self-healing backfill:
any projection-logic change redeploys through it with zero provider
calls in steady state, so "re-run the refresh" is the answer to most
catalog-shape drift. A raw whose `fields_version` trails the running
build, or that predates the field entirely, is refetched once as
part of the same sweep - a set that drains to zero as the catalog
heals, so shipping a new IGDB field costs one refresh's worth of
provider calls and nothing after. `gamePayloadFor` runs the same
check on resolve and promote (the paths that build a fresh IGDB
projection), so a single product can heal ahead of its next nightly
refresh too; the plain `GET /products/{id}` staleness refetch is a
separate, age-only mechanism (`IGDB_REFRESH_AFTER`) that does not
look at `fields_version`.

Normalize community regions (admin role or a service token): promotes
free-text community-product regions into the known set
(exact-or-synonym folding against `regionkit.KnownRegions`/
`regionkit.RegionSynonyms`, never fuzzy - an unreviewed string is
left as typed). This service's
twin of collection's normalize-regions lever, scoped to the community
products enrichment owns; no fetch arm, since a community product
carries no provider identity to re-fetch, so promotion is a plain
region field rewrite. Scheduled nightly in the enrichment-refresh
CronJob's chain, right after the catalog refresh trigger, and
operator-runnable at any other time with an admin bearer or the bff
Admin page's "Run community region normalization" button (`POST
/api/admin/normalize-community-regions`, relayed to this same
endpoint). Bruno: `bruno/enrichment/normalize-community-regions.bru`,
or:

```bash
curl -X POST -H "Authorization: Bearer $ADMIN_JWT" \
  http://localhost:8084/internal/normalize-community-regions
```

Answers `{"scanned":N,"normalized":N,"skipped":N}`. Idempotent: a
promoted row leaves the selection set, so a second run normalizes 0
once nothing unreviewed remains.

Moderated mapping fix (validates against the provider, snapshots,
marks verified; `{}` clears the mapping):

```bash
curl -X PUT -H "Authorization: Bearer $ADMIN_JWT" -H "Content-Type: application/json" \
  -d '{"pc_product_id": 6910}' \
  http://localhost:8084/admin/products/<product-uuid>/pricecharting
```

Worklists and community moderation, same auth:

- `GET /admin/products/unmatched` - products with no listing mapping
  (includes admin-held ones).
- `GET /admin/products/community` - admin-minted community products.
- `POST /admin/products` - mint a community product.
- `POST /admin/products/{id}/promote` - attach provider anchors and
  flip a community product to provider origin in place (id stable, so
  every adopter upgrades through live reads).
- `GET /admin/products/promote-candidates` and
  `POST /admin/products/{id}/promote-candidates/dismiss` - review or
  permanently silence the sweep's suggestions.
- `DELETE /admin/products/{id}` - remove unmatched residue; matched
  products refuse with 409 until cleared.

Internal token rotation no longer happens here: the CronJob's shared
secret lives entirely at auth's `/internal/service-token` now (see
[auth.md](auth.md)'s Admin levers for the A/B rotation procedure).
This service holds no internal secret of its own to rotate.

## Capacity and rollout

One replica (`replicas: 1`), requests 50m CPU / 64Mi, memory limit
128Mi, no CPU limit. The heaviest allocation path is recommendation
scoring (2500-entry libraries, 200 candidates); watch "Working-set
memory by pod" before trimming the limit. Postgres: 50m / 128Mi
requests, 256Mi limit, 1Gi PVC. Valkey: 50m / 64Mi requests, 128Mi
limit.

PDBs set `minAvailable: 1` on every workload. With single
replicas that means voluntary drains block instead of silently
dropping the only copy - intentional; scale before draining, or
accept the eviction being refused.

Probes: liveness `/healthz`, readiness `/readyz`, chart sets no
explicit timings so kubernetes defaults apply (10s period, 3-failure
threshold); postgres and valkey ready-check via their own CLIs every
5s.

A rollout surges: deployment defaults round to maxSurge 1 /
maxUnavailable 0 at one replica, so the new pod starts, runs the
migrate init container, and must pass readyz (Postgres ping) before
the old pod terminates. Traffic never drops as long as the new pod goes
ready. What does not survive the swap: the Valkey-independent
in-process refresh guard, and any detached refresh mid-flight - the
refresh dies unlogged with the old process, so re-trigger the refresh
after rolling during one (nothing corrupts; the refresh is per-product
idempotent and snapshots are append-only, at worst a same-day
duplicate snapshot per product).

CronJob shape: schedule `0 6 * * *`, concurrencyPolicy Forbid (the
service's 409 guard is the inner layer), startingDeadlineSeconds 3600,
backoffLimit 2, activeDeadlineSeconds 900 for the curl pod itself,
which runs `&&`-joined hops after the token exchange
(`--max-time 60` each): the refresh trigger - detached and budgeted at
30m inside the service - then normalize-community-regions, which runs
to completion synchronously before the pod exits. The script runs
under `set -e -o pipefail`, so a failed token exchange (the `sed`
after the curl piping into it) aborts the job instead of running the
later hops on an empty token.

Datastore restarts: Postgres restarting takes enrichment unready until
the ping passes again (failure mode 2). Valkey restarting costs
only cache warmth. Neither requires restarting enrichment itself
except the boot-time Valkey dependency noted earlier.
