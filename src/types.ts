import { DataSourceJsonData } from '@grafana/data';
import { DataQuery } from '@grafana/schema';

export type AuthType = 'sql' | 'ntlm' | 'kerberos';

export interface ConnectionOptions {
  host: string;
  port: string;
  instanceName?: string; // named instance, e.g. "SCOM"; leave blank for the default instance
  database: string;
  authType: AuthType;
  username?: string;
  domain?: string; // ntlm
  realm?: string; // kerberos
  encrypt: string; // 'true' | 'false' | 'disable' | 'strict'
  trustServerCertificate: boolean;
}

export const DEFAULT_CONNECTION: ConnectionOptions = {
  host: '',
  port: '1433',
  database: '',
  authType: 'sql',
  encrypt: 'true',
  trustServerCertificate: false,
};

/**
 * These are options configured for each DataSource instance
 */
export interface MyDataSourceOptions extends DataSourceJsonData {
  operational: ConnectionOptions;
  warehouse: ConnectionOptions;
  warehouseSameAsOperational?: boolean;
}

/**
 * Values that are used in the backend, but never sent over HTTP to the frontend
 */
export interface MySecureJsonData {
  operationalPassword?: string;
  warehousePassword?: string;
}

export type SCOMQueryType = 'performance' | 'alerts' | 'health' | 'properties' | 'health-tree';

export type ClassSearchBy = 'name' | 'displayName';

export type Aggregation = 'raw' | 'hourly' | 'daily';

// Which frames a Health query returns. Left unset, the backend treats it the
// same as 'both' — see HealthMode in pkg/scom/query.go.
export type HealthMode = 'current' | 'history' | 'both';

// Which database an Alerts query reads from. Left unset, the backend treats
// it the same as 'operational' — see AlertSource in pkg/scom/query.go. The
// warehouse keeps alert history far longer than the Operational database,
// at the cost of the Owner field and an approximated last-modified time —
// see QueryAlertsWarehouse in pkg/scom/alerts.go.
export type AlertSource = 'operational' | 'warehouse';

export interface ResourceRef {
  value: string;
  label: string;
}

// class/group/instances/counters carry {value,label} pairs (rather than bare
// ids) so the query editor can redisplay a saved query's selections without
// a round trip to re-resolve a GUID into a human-readable label.
export interface MyQuery extends DataQuery {
  queryType: SCOMQueryType;
  class?: ResourceRef;
  group?: ResourceRef;
  // 'health-tree' normally requires exactly one instance here (see
  // filterQuery in datasource.ts and healthTreeInstanceID in
  // pkg/scom/query.go) — it renders a single object's full SCOM monitor
  // tree, unlike every other query type where "zero instances" means
  // "everything in scope." The one exception is a group scope with no
  // instances narrowed down: that renders the group's own health rollup
  // (see the QueryTypeHealthTree branch in pkg/scom/query.go's Run).
  instances?: ResourceRef[];
  // Performance counters are picked in three steps: object (e.g. "Process"),
  // then counter name (e.g. "Working Set") within that object, then
  // optionally specific instances (e.g. "cshost") to narrow further. With
  // object/counterName chosen but counters empty, the query runs against
  // every instance reporting that counter.
  object?: ResourceRef;
  counterName?: ResourceRef;
  counters?: ResourceRef[];
  aggregation?: Aggregation;
  // Overrides the default series legend ("Object - Counter [Instance]
  // (Entity)"), which reads every field there is and gets unwieldy fast.
  // Supports {{object}}, {{counter}}, {{instance}}, {{entity}} macros; empty
  // (including on a query saved before this field existed) keeps the
  // built-in default — see seriesLabel in pkg/scom/performance.go.
  legendFormat?: string;
  // Ignores class/group/instances entirely and returns every alert in the
  // time range (still subject to severity/resolution state filters below).
  allAlerts?: boolean;
  severityFilter?: number[];
  resolutionStateFilter?: number[];
  alertSource?: AlertSource;
  propertyNames?: string[];
  healthMode?: HealthMode;
  // Prunes a health tree query down to monitors that are themselves
  // Warning/Critical, plus their ancestor chain back to the root — see
  // filterUnhealthyBranches in pkg/scom/healthtree.go. Ignored by every
  // other query type.
  unhealthyOnly?: boolean;
}

export const DEFAULT_QUERY: Partial<MyQuery> = {
  queryType: 'performance',
  aggregation: 'hourly',
  // 0 = New, one of SCOM's built-in resolution states (guaranteed present,
  // unlike custom ones). An alerts query with nothing scoped runs against
  // every alert in the management group, so this keeps a fresh query cheap
  // by default — broaden it by clearing the resolution state filter.
  resolutionStateFilter: [0],
};

// Severity is a fixed 3-value SCOM enum, unlike resolution state (see
// AlertFilters.tsx), which administrators can extend — so only this one is
// safe to hardcode.
export const ALERT_SEVERITY_OPTIONS = [
  { label: 'Information', value: 0 },
  { label: 'Warning', value: 1 },
  { label: 'Critical', value: 2 },
];
