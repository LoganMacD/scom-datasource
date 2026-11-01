# SCOM data source — project notes for agents

Grafana **backend data source** plugin (`scom-datasource`) that queries Microsoft SCOM 2022 by connecting **directly to SQL Server**: the Operational DB (`OperationsManager`) and the Data Warehouse (`OperationsManagerDW`). No SCOM SDK/REST API is involved — every feature is a hand-written T-SQL query.

Kept outside `.config/` on purpose so `create-plugin update` doesn't overwrite it. Generic Grafana plugin rules live in [.config/AGENTS/instructions.md](.config/AGENTS/instructions.md).

## Layout

| Path | What it is |
| --- | --- |
| `pkg/main.go` | Entry point, `datasource.Manage("scom-datasource", ...)` |
| `pkg/models/settings.go` | `PluginSettings` (jsonData) + `SecretPluginSettings` (secureJsonData) |
| `pkg/plugin/datasource.go` | `QueryData`, `CheckHealth`, `CallResource` (picker endpoints) |
| `pkg/scom/query.go` | `QueryModel` + `Run()` — dispatches each query type, resolves class/group scope |
| `pkg/scom/db.go`, `conn.go` | Opens both `*sql.DB` connections, builds DSN (SQL / NTLM / Kerberos auth) |
| `pkg/scom/<domain>.go` | One file per feature: `performance`, `alerts`, `health`, `healthtree`, `properties`, `classes`, `groups`, `instances`, `counters`, `relationships` |
| `pkg/scom/sql_helpers.go` | Shared SQL builders — reuse these, don't reinvent them |
| `src/types.ts` | `MyQuery`, `MyDataSourceOptions`, defaults |
| `src/datasource.ts` | `filterQuery`, template vars, `getResource` wrappers |
| `src/components/` | `QueryEditor`, `ConfigEditor`, `ConnectionSettingsEditor`, pickers |
| `tests/` | Playwright e2e smoke tests (`@grafana/plugin-e2e`) |
| `provisioning/datasources/datasources.yml` | Dev data source for `npm run server` |
| `SQL Schema/SCOM 2022/` | **Read-only** schema reference: `OperationsManager/` and `OperationsManagerDW/`, one `.sql` per table/view/function, named `<schema>.<object>.<Type>.sql` (e.g. `CS.ManagedEntity.Table.sql`, `Perf.vPerfHourly.View.sql`) |

## Which database answers what

- **Operational** (`db.Operational`): classes, groups, instances, alerts (default), current health, health history, health tree, properties, resolution states. Key tables: `dbo.BaseManagedEntity`, `dbo.ManagedType`, `dbo.DerivedManagedTypes`, `dbo.Relationship`, `dbo.State`, `dbo.Monitor`, `dbo.LocalizedText`, `dbo.Alert`.
- **Warehouse** (`db.Warehouse`): performance (`Perf.vPerfRaw` / `vPerfHourly` / `vPerfDaily`, `dbo.vPerformanceRule`, `dbo.vPerformanceRuleInstance`, `dbo.vManagedEntity`) and alerts when `alertSource = 'warehouse'` (`Alert.vAlert`).
- Before writing or changing a query, grep `SQL Schema/` for the table/view to confirm column names and types. Don't guess from memory.

## Query types (`queryType`)

`performance`, `alerts`, `health`, `properties`, `health-tree`. When you add or change one, these have to stay in sync:

1. `src/types.ts` `MyQuery` ⇄ `pkg/scom/query.go` `QueryModel` (same JSON field names).
2. `src/datasource.ts` `filterQuery` (frontend "is this runnable" rule).
3. `pkg/scom/query.go` `Run()` (backend dispatch + validation; drilldown links bypass `filterQuery`, so the backend has to enforce the rules too).
4. `src/components/QueryEditor.tsx` UI.

Picker resource endpoints (`CallResource`): `classes`, `groups`, `instances`, `counter-objects`, `counter-names`, `counters`, `properties`, `resolution-states`. Each returns a JSON array of `{value,label}`. Always send `[]`, never `null`.

## Domain rules that aren't obvious

- **Scope resolution**: no instances picked means every instance in the chosen class (including subclasses, via `DerivedManagedTypes`) and/or group (`ResolveInstanceIDs`). Alerts with nothing scoped, or with `allAlerts`, ignore scope entirely.
- **Hosting expansion**: perf counters and alerts usually live on *hosted* children (disks, DB engines, services), not the selected object. Run `ExpandHostedEntityIDs` on the scope before you filter perf/alerts.
- **`PerformanceRuleInstanceRowId` is not entity-specific.** It's keyed on (rule, instance name) and shared by every entity. Always also filter by entity on the fact row, and split series per entity.
- **Counter pickers sample** the first `CounterPickerSampleSize` (10) instances. Query execution never samples.
- **Perf aggregation**: `auto` (the default for new queries) picks a single stored grain per request in `choosePerfGrain` (`pkg/scom/perfgrain.go`), using live DW retention (`dbo.StandardDatasetAggregation.MaxDataAgeDays`, matched by storage base table name) and Grafana's MaxDataPoints/Interval. Explicit `raw`/`hourly`/`daily` are never overridden. An unset aggregation means `raw` (legacy). Never stitch grains together.
- **Health tree** requires exactly one instance, or a group with no instances (rooted at the group, with members underneath).
- **Backward compatibility**: new `QueryModel` fields must make the zero value / unset mean the old behaviour, because saved dashboards don't have the field. Existing examples: `HealthMode` "" means both, `AlertSource` "" means operational, `LegendFormat` "" means the default label.

## SQL conventions (follow them, they fix real production failures)

- **Always parameterize** with `sql.Named`. For variable-length lists use `inClause` for small lists, and `idScopeTempTable` (temp table + one `STRING_SPLIT` VARCHAR(MAX) param) for any list that can be large. Big IN lists hit SQL Server's ~2,100 param cap and "query processor ran out of internal resources".
- Temp table names are fixed constants (`#EntityScope`, `#CounterScope`, `#HealthTreeScope`, ...). That's safe because they're connection-scoped.
- **Display names**: use `localizedNameApply` (OUTER APPLY TOP 1 with language preference ENU → ENA → any) plus `ISNULL(alias.LTValue, <internal name>)`. Never filter `LocalizedText` to one language, and avoid SCOM's `*View` wrappers that fan out per language.
- `monitorOperationalStateApply` instead of joining `dbo.MonitorOperationalState` (it isn't unique per monitor/health state).
- Free-text search: `searchLikePattern(search)` together with `LIKE @search ESCAPE '\'` (contains-match; users can type `*`/`?` wildcards). Cap results with `defaultSearchLimit`.
- `sessionInitSQL` (ANSI_NULLS, ARITHABORT, ...) runs on every connection so SCOM's indexed views get used. Don't remove it.
- Build each query in a pure `buildXxxQuery(...) (string, []any)` function, separate from execution, so `*_test.go` can assert on the SQL/args without a live DB. Follow this pattern for new queries.

## Config / secrets

- `jsonData`: `operational` and `warehouse` (`ConnectionSettings`: host, port, instanceName, database, authType `sql|ntlm|kerberos`, username, domain, realm, encrypt, trustServerCertificate) and `warehouseSameAsOperational`.
- `secureJsonData`: `operationalPassword`, `warehousePassword`.
- `CheckHealth` probes `dbo.BaseManagedEntity` (Operational) and `Perf.vPerfHourly` (Warehouse).

## Commands

```sh
npm run typecheck && npm run lint && npm run test:ci   # frontend checks (same as CI)
npm run build                                          # frontend production build (webpack)
go test ./pkg/...                                      # backend unit tests (no DB needed)
mage -v                                                # backend binaries for all platforms
npm run server                                         # Grafana in Docker with the plugin + provisioning
npm run e2e                                            # Playwright (needs `npm run server` running)
```

CI (`.github/workflows/ci.yml`) also runs `golangci-lint` (config in `.golangci.yml`).

There's no local SCOM database, so e2e tests only cover rendering and failed connections. Check query logic through the Go `buildXxxQuery` tests.

## Releases

Versions are cut with `npm version <major|minor|patch>`, which commits `X.Y.Z` and pushes the tag (`postversion`), and that tag triggers `release.yml`. Feature commits are plain descriptive messages. Don't bump versions unless asked.
