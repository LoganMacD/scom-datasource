# SCOM data source — project notes for agents

Grafana **backend data source** plugin (`scom-datasource`) that queries Microsoft SCOM 2022 by connecting **directly to SQL Server**: the Operational DB (`OperationsManager`) and the Data Warehouse (`OperationsManagerDW`). No SCOM SDK/REST API is involved — every feature is a hand-written T-SQL query.

Kept outside `.config/` on purpose so `create-plugin update` doesn't overwrite it. Generic Grafana plugin rules live in [.config/AGENTS/instructions.md](.config/AGENTS/instructions.md).

## Layout

| Path | What it is |
| --- | --- |
| `pkg/main.go` | Entry point, `datasource.Manage("scom-datasource", ...)` |
| `pkg/models/settings.go` | `PluginSettings` (jsonData) + `SecretPluginSettings` (secureJsonData) |
| `pkg/plugin/datasource.go` | `QueryData` (runs a request's queries in parallel), `CheckHealth`, `CallResource` (picker and variable endpoints), error status/source mapping |
| `pkg/scom/query.go` | `QueryModel`, `validate()`, `Run()` — dispatches each query type, resolves class/group scope |
| `pkg/scom/db.go`, `conn.go` | Opens both `*sql.DB` connections (pool limits), builds DSN (SQL / NTLM / Kerberos auth), `CheckHealth` |
| `pkg/scom/cache.go`, `scope.go` | Per-data-source TTL caches with singleflight; cached scope resolution (`InstanceScope`, `ExpandedScope`, `CounterPickerScope`) |
| `pkg/scom/<domain>.go` | One file per feature: `performance`, `perfgrain`, `alerts`, `health` (incl. timeline), `healthtree`, `properties`, `classes`, `groups`, `instances`, `counters`, `relationships` |
| `pkg/scom/sql_helpers.go` | Shared SQL builders — reuse these, don't reinvent them |
| `pkg/scom/search.go`, `errors.go`, `colors.go` | In-memory search of cached lists; `BadRequestError`/`ValidateIDs`/`IsDownstreamError`/`ConnectionHint`; value mappings for state colors |
| `src/types.ts` | `MyQuery`, `SCOMVariableQuery`, `MyDataSourceOptions`, defaults |
| `src/datasource.ts` | `filterQuery`, `applyTemplateVariables`, `getQueryDisplayText`, `metricFindQuery`, `getResource` wrappers |
| `src/templating.ts`, `variables.ts`, `annotations.ts` | Variable interpolation (multi-value expansion), dashboard variable support, alerts/health-history annotations |
| `src/components/` | `QueryEditor`, `VariableQueryEditor`, `ConfigEditor`, `ConnectionSettingsEditor`, pickers, `useLoadError` |
| `src/*.test.ts`, `src/testing/` | Jest unit tests and a fake template service |
| `tests/` | Playwright e2e smoke tests (`@grafana/plugin-e2e`): config, query, variable and annotation editors |
| `provisioning/datasources/datasources.yml` | Dev data source for `npm run server` |
| `SQL Schema/SCOM 2022/` | **Read-only** schema reference: `OperationsManager/` and `OperationsManagerDW/`, one `.sql` per table/view/function, named `<schema>.<object>.<Type>.sql` (e.g. `CS.ManagedEntity.Table.sql`, `Perf.vPerfHourly.View.sql`) |

## Which database answers what

- **Operational** (`db.Operational`): classes, groups, instances, alerts (default), current health, health history, health tree, properties, resolution states. Key tables: `dbo.BaseManagedEntity`, `dbo.ManagedType`, `dbo.DerivedManagedTypes`, `dbo.Relationship`, `dbo.State`, `dbo.Monitor`, `dbo.LocalizedText`, `dbo.Alert`.
- **Warehouse** (`db.Warehouse`): performance (`Perf.vPerfRaw` / `vPerfHourly` / `vPerfDaily`, `dbo.vPerformanceRule`, `dbo.vPerformanceRuleInstance`, `dbo.vManagedEntity`) and alerts when `alertSource = 'warehouse'` (`Alert.vAlert`).
- Before writing or changing a query, grep `SQL Schema/` for the table/view to confirm column names and types. Don't guess from memory.
- The schema files are **UTF-16**, so a plain grep/cat shows spaced-out text. Read them with `iconv -f UTF-16 -t UTF-8 <file>`.
- **Grafana always queries the `vPerf*` views, never the physical tables.** `Perf.vPerfRaw/Hourly/Daily` sit over environment-specific tables (`Perf.PerfRaw_<guid>` etc.), and the GUID differs between environments. Raw has `SampleValue`. Hourly and daily have `SampleCount`, `AverageValue`, `MinValue`, `MaxValue` and `StandardDeviation`; averaging averages without weighting by `SampleCount` is wrong. Don't use the Operational DB's `PerformanceData_*` tables for history.
- **DW retention metadata** (read by `loadPerfRetention`):
  - `dbo.StandardDataset` (filter `SchemaName = 'Perf'`) joins `dbo.StandardDatasetAggregation` on `DatasetId`. That table holds `MaxDataAgeDays`, `AggregationIntervalDurationMinutes` (NULL for raw) and `LastGroomingDateTime`.
  - That joins `dbo.StandardDatasetAggregationStorage` on `DatasetId` + `AggregationTypeId`. Its `BaseTableName` (`PerfRaw`/`PerfHourly`/`PerfDaily`) identifies each grain.
  - The schema doesn't seed `AggregationTypeId` values, so identify grains by table name, not ID. Never hardcode retention days.
  - `dbo.StandardDatasetTableMap` has per-table `StartDateTime`/`EndDateTime` if you ever need actual coverage rather than the retention setting.
- **Partition indexes aren't in the schema export.** It only scripts each partition table's clustered identity PK; SCOM creates the real indexes at runtime from `dbo.StandardDatasetAggregationStorageIndex.IndexDefinition` (joined to `dbo.StandardDatasetAggregationStorage` on `StandardDatasetAggregationStorageRowId`). Read that table, or `sys.indexes` on `Perf.PerfRaw_*` / `Alert.AlertResolutionState_*`, before tuning a fact-table query.
- **Nullable columns that bite:** `dbo.vManagedEntity.ManagedEntityDefaultName` (computed from the nullable `DisplayName`; fall back with `COALESCE(..., me.Name, guid)`), `dbo.Alert.TimeRaised` (fall back to `TimeAdded`), `Perf.vPerfRaw.SampleValue` (skip NULL samples).
- **Not yet confirmed against a live DW:** whether hourly/daily `DateTime` marks the start or the end of its bucket, the real retention values, whether the plugin's warehouse account can read the retention tables (Save & test now warns if not), and the partition index layout. Don't build anything that depends on these (e.g. stitching grains) until they're confirmed.

## Query types (`queryType`)

`performance`, `alerts`, `health`, `properties`, `health-tree`. When you add or change one, these have to stay in sync:

1. `src/types.ts` `MyQuery` ⇄ `pkg/scom/query.go` `QueryModel` (same JSON field names).
2. `src/datasource.ts` `filterQuery` (frontend "is this runnable" rule).
3. `pkg/scom/query.go` `validate()` and `Run()` (backend dispatch + validation; drilldown links bypass `filterQuery`, so the backend has to enforce the rules too).
4. `src/components/QueryEditor.tsx` UI, and `getQueryDisplayText` in `src/datasource.ts`.
5. `applyTemplateVariables` in `src/datasource.ts` if the field can hold an id or name a dashboard variable could stand in for.

Resource endpoints (`CallResource`): `classes`, `groups`, `instances`, `counter-objects`, `counter-names`, `counters`, `properties`, `resolution-states`, and `variable-values?type=classes|groups|instances` (uncapped lists for dashboard variables; instances capped at `variableValuesLimit`). Each returns a JSON array of `{value,label[,description]}` — `classes` puts the technical TypeName in `description`. Always send `[]`, never `null`. Ids are checked with `ValidateIDs` first, so a bad or unresolved `$variable` is a 400 with a readable message.

Newer query options (all unset = old behaviour): `statistic` (`average|min|max` for hourly/daily; "" = average), `alertTimeFilter` (`raised|active|ignore`; "" = raised; the warehouse source only does `raised` and adds a notice otherwise), `healthMode: 'timeline'` (per-entity state series for a State timeline panel), `stateColors` (value mappings for health state, severity, priority, maintenance mode; on in `DEFAULT_QUERY`, off for saved queries because data-source mappings override a panel's own).

## Domain rules that aren't obvious

- **Scope resolution**: no instances picked means every instance in the chosen class (including subclasses, via `DerivedManagedTypes`) and/or group (`ResolveInstanceIDs`). Go through the cached `DB.InstanceScope` / `DB.ExpandedScope`, not the raw functions. Alerts with nothing scoped, or with `allAlerts`, ignore scope entirely.
- **An empty scope returns nothing, with a notice** (`emptyScopeNotice`). It used to apply no filter and return every entity's perf data / every alert.
- **Hosting expansion**: perf counters and alerts usually live on *hosted* children (disks, DB engines, services), not the selected object. `DB.ExpandedScope` widens the scope (a class/group resolves and expands in one batch, `ExpandScopeIDs`) before perf/alerts filter on it.
- **`PerformanceRuleInstanceRowId` is not entity-specific.** It's keyed on (rule, instance name) and shared by every entity. Always also filter by entity on the fact row (the scope applies to explicitly picked counters too), and split series per entity.
- **Counter pickers sample** the first `CounterPickerSampleSize` (10) instances by display name (`TOP` in SQL, so the sample is stable). Query execution never samples.
- **Perf aggregation**: `auto` (the default for new queries) picks a single stored grain per request in `choosePerfGrain` (`pkg/scom/perfgrain.go`), using live DW retention (`dbo.StandardDatasetAggregation.MaxDataAgeDays`, matched by storage base table name) and Grafana's MaxDataPoints/Interval. Explicit `raw`/`hourly`/`daily` are never overridden. An unset aggregation means `raw` (legacy). Never stitch grains together.
- **Health tree** requires exactly one instance, or a group with no instances (rooted at the group, with members underneath).
- **Backward compatibility**: new `QueryModel` fields must make the zero value / unset mean the old behaviour, because saved dashboards don't have the field. Existing examples: `HealthMode` "" means both, `AlertSource` "" means operational, `LegendFormat` "" means the default label, `Statistic` "" means average, `AlertTimeFilter` "" means raised, `StateColors` false means uncolored.
- **Perf series order** is by legend (then ids), so legends and Grafana's position-based colors don't shuffle between refreshes.

## Caching and connections

- Each data source instance (`scom.DB`) has its own caches (`dbCaches`); they're dropped when its settings are saved. Sharing results between users is safe because every user of a data source uses the same SQL logins.
- TTLs (`cache.go`): scope resolution and counter-instance searches 5 min (`scopeCacheTTL`); retention, resolution states, class and group lists, class property columns, counter object/name lists 15 min (`metadataCacheTTL`); a failed retention read 1 min. Errors otherwise aren't cached, and a load cancelled by its own request is retried by anyone waiting on it.
- Concurrent requests for the same key share one load (singleflight), so a dashboard's panels resolve a shared group once. Caches are bounded by item count (`maxCachedIDs` etc.). Cached slices are shared — never modify them.
- Classes, groups and counter objects/names are loaded whole and searched in memory (`searchOptions`, same `*`/`?` semantics as `searchLikePattern`, exact and prefix matches first). Instances and counter instances still search in SQL (lists can be huge).
- Pool per database: 20 open, 10 idle, 10 min idle timeout, 1 h lifetime (`db.go`). Don't hold a connection open while querying the same pool again — with a cap that can deadlock.
- `QueryData` runs up to 4 queries of one request in parallel (`experimental/concurrent`); Health current and history run in parallel too.

## SQL conventions (follow them, they fix real production failures)

- **Always parameterize** with `sql.Named`. For variable-length lists use `inClause`/`int64InClause` for small lists, and `idScopeTempTable` / `intScopeTempTable` (temp table + one `STRING_SPLIT` VARCHAR(MAX) param; give each int list in a batch its own param name) for any list that can be large. Big IN lists hit SQL Server's ~2,100 param cap and "query processor ran out of internal resources".
- **Perf fact queries read ids, time and value only** (`buildPerformanceQuery`), filter on the fact view's own columns (`#EntityRows`/`#CounterRows` row-id temp tables), and have no `ORDER BY`; names come from one follow-up lookup per series (`buildPerformanceSeriesQuery`, two result sets). Don't join names back onto fact rows.
- Warehouse alerts put the in-range alerts in `#AlertRows` first and compute the latest resolution state for those only.
- Temp table names are fixed constants (`#EntityScope`, `#CounterScope`, `#HealthTreeScope`, ...). That's safe because they're connection-scoped.
- **Display names**: use `localizedNameApply` (OUTER APPLY TOP 1 with language preference ENU → ENA → any) plus `ISNULL(alias.LTValue, <internal name>)`. Never filter `LocalizedText` to one language, and avoid SCOM's `*View` wrappers that fan out per language.
- `monitorOperationalStateApply` instead of joining `dbo.MonitorOperationalState` (it isn't unique per monitor/health state).
- Free-text search: `searchLikePattern(search)` together with `LIKE @search ESCAPE '\'` (contains-match; users can type `*`/`?` wildcards). Cap results with `defaultSearchLimit`.
- `sessionInitSQL` (ANSI_NULLS, ARITHABORT, ...) runs on every connection so SCOM's indexed views get used. Don't remove it.
- Build each query in a pure `buildXxxQuery(...) (string, []any)` function, separate from execution, so `*_test.go` can assert on the SQL/args without a live DB. Follow this pattern for new queries.

## Config / secrets

- `jsonData`: `operational` and `warehouse` (`ConnectionSettings`: host, port, instanceName, database, authType `sql|ntlm|kerberos`, username, domain, realm, encrypt, trustServerCertificate) and `warehouseSameAsOperational`.
- `warehouseSameAsOperational` shares the Operational **server and credentials** only; the warehouse keeps its own `database` (default `OperationsManagerDW`, `models.DefaultWarehouseDatabase`). Copying the database name too used to point every warehouse query at OperationsManager.
- `secureJsonData`: `operationalPassword`, `warehousePassword`.
- `CheckHealth` checks both databases in parallel (ping, then `dbo.BaseManagedEntity` / `Perf.vPerfHourly`), reports each in `details.message` with a `ConnectionHint`, and returns OK-with-warning when the warehouse account can't read the retention tables.

## Errors

- Query problems (bad ids, unknown options, wrong instance count) are `scom.BadRequestError` (`badRequestf`) → HTTP 400, error source downstream. SQL Server, network and timeout failures (`IsDownstreamError`) → downstream; anything else → plugin error. Keep wrapping with `%w` so the classification survives.
- Notices (`data.Notice` on the first frame) explain empty or adjusted results; prefer one over an empty panel.
- The perf diagnostics line logs at Debug, or Info when Auto falls back or the range predates retention.

## Frontend conventions

- Every discrete editor change calls `onChange` then `onRunQuery`; `filterQuery` skips queries that aren't runnable. Free text (legend) runs on blur.
- Pickers offer dashboard variables (`variableOptions`) and show load failures under the field via `useLoadError`. Resource calls interpolate scope values first (`interpolateValues` expands multi-value variables).
- `"annotations": true` in `plugin.json` enables annotations; `annotationSupport.processEvents` turns `alerts` and `healthHistory` frames into events. Any `plugin.json` change needs a Grafana server restart.

## Commands

```sh
npm run typecheck && npm run lint && npm run test:ci   # frontend checks (same as CI)
npm run build                                          # frontend production build (webpack)
go test ./pkg/...                                      # backend unit tests (no DB needed)
mage -v                                                # backend binaries for all platforms
npm run server                                         # Grafana in Docker with the plugin + provisioning
npm run e2e                                            # Playwright (needs `npm run server` running)
```

CI (`.github/workflows/ci.yml`) also runs `golangci-lint` (config in `.golangci.yml`). It isn't installed locally. Run it with `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...`.

### Windows dev machine quirks

- **gofmt line endings:** the checkout uses CRLF (`core.autocrlf`), so `gofmt -l pkg` lists *every* file. That's just the line endings, not a real formatting problem, and CI's Linux checkout doesn't have it. To check a file, strip the CRs first: `tr -d '\r' < file.go | gofmt -d`.
- **Prettier:** `prettier --check src` already fails on several files at HEAD, so don't mass-reformat. CI only runs eslint.
- **Tooling:** `gh` is installed (and logged in); there's no SQL client or Docker on PATH, so e2e tests only run in CI. `go test -race` needs cgo, which isn't set up. Check CI runs with `gh run list` or at https://github.com/LoganMacD/scom-datasource/actions.
- **Git identity:** none is configured. Commit as `LoganMacD <me@a-logan.com>` (matches history) using `GIT_AUTHOR_*`/`GIT_COMMITTER_*` env vars, without changing git config.

There's no local SCOM database, so e2e tests only cover rendering and failed connections. Check query logic through the Go `buildXxxQuery` tests.

## Releases

Versions are cut with `npm run release -- <major|minor|patch>` (`npm version`), which commits `X.Y.Z` and creates tag `vX.Y.Z`. Pushing a `v*` tag triggers `release.yml`.

**`.npmrc` has `ignore-scripts=true`, so the `postversion` push hook never runs.** Push by hand afterwards: `git push && git push origin vX.Y.Z`.

Feature commits are plain descriptive messages; the version commit is just `X.Y.Z`. Use a minor bump for features and a patch for fixes. Don't bump versions or push unless asked.
