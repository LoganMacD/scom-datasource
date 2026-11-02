import {
  CoreApp,
  DataSourceInstanceSettings,
  LegacyMetricFindQueryOptions,
  MetricFindValue,
  ScopedVars,
} from '@grafana/data';
import { DataSourceWithBackend, getTemplateSrv } from '@grafana/runtime';

import { annotationSupport } from './annotations';
import { interpolateRef, interpolateRefs, interpolateValue, interpolateValues } from './templating';
import {
  ALERT_SEVERITY_OPTIONS,
  DEFAULT_QUERY,
  MyDataSourceOptions,
  MyQuery,
  ResourceRef,
  SCOMVariableQuery,
} from './types';
import { SCOMVariableSupport } from './variables';

export interface ResourceOption {
  value: string;
  label: string;
  description?: string;
}

const AGGREGATION_LABELS: Record<string, string> = { auto: 'Auto', raw: 'Raw', hourly: 'Hourly', daily: 'Daily' };
const ALERT_TIME_LABELS: Record<string, string> = {
  raised: 'raised in range',
  active: 'active in range',
  ignore: 'any time',
};

function refLabels(refs?: ResourceRef[]): string {
  return (refs ?? []).map((r) => r.label || r.value).join(', ');
}

export class DataSource extends DataSourceWithBackend<MyQuery, MyDataSourceOptions> {
  constructor(instanceSettings: DataSourceInstanceSettings<MyDataSourceOptions>) {
    super(instanceSettings);
    this.variables = new SCOMVariableSupport(this);
    this.annotations = annotationSupport;
  }

  getDefaultQuery(_: CoreApp): Partial<MyQuery> {
    return DEFAULT_QUERY;
  }

  // Dashboard variables can stand in for any picked class, group, instance
  // or counter, and for the object/counter names and the legend. A
  // multi-value variable expands into one selection per value.
  applyTemplateVariables(query: MyQuery, scopedVars: ScopedVars): MyQuery {
    return {
      ...query,
      class: interpolateRef(query.class, scopedVars),
      group: interpolateRef(query.group, scopedVars),
      instances: interpolateRefs(query.instances, scopedVars),
      object: interpolateRef(query.object, scopedVars),
      counterName: interpolateRef(query.counterName, scopedVars),
      counters: interpolateRefs(query.counters, scopedVars),
      legendFormat: query.legendFormat ? getTemplateSrv().replace(query.legendFormat, scopedVars) : query.legendFormat,
    };
  }

  filterQuery(query: MyQuery): boolean {
    switch (query.queryType) {
      case 'performance':
        // No counter instances narrowed down still runs, scoped to every
        // instance reporting the chosen object/counter; only truly nothing
        // selected is filtered out.
        return !!(query.counters?.length || (query.object && query.counterName));
      case 'alerts':
        // Unlike the other query types, alerts always runs: with nothing
        // scoped (or "All alerts" checked), the backend returns every alert
        // rather than treating it as "no data."
        return true;
      case 'health':
      case 'properties':
        // No instances picked still runs, scoped to every instance under the
        // chosen class/group; only truly nothing selected is filtered out.
        return !!(query.instances?.length || query.class || query.group);
      case 'health-tree':
        // A health tree is inherently about one object, so it runs with
        // exactly one instance picked — a class scope resolving to several
        // instances isn't enough. The one exception is a group scope with no
        // instances narrowed down, which shows the group's own health
        // rollup. The backend enforces both rules too (the QueryTypeHealthTree
        // branch in pkg/scom/query.go's Run), since a drilldown link's query
        // payload bypasses this frontend check entirely.
        return query.instances?.length === 1 || (!!query.group && !query.instances?.length);
      default:
        return false;
    }
  }

  // A one-line summary for Explore's query history and the inspector, instead
  // of the query's JSON.
  getQueryDisplayText(query: MyQuery): string {
    const scope = query.instances?.length
      ? refLabels(query.instances)
      : query.class
        ? `class ${query.class.label}`
        : query.group
          ? `group ${query.group.label}`
          : '';
    const parts: string[] = [];

    switch (query.queryType) {
      case 'performance': {
        const counter = [query.object?.label, query.counterName?.label].filter(Boolean).join(' / ');
        const instances = query.counters?.length ? ` [${refLabels(query.counters)}]` : '';
        parts.push(`Performance: ${counter || 'no counter'}${instances}`, scope);
        const aggregation = AGGREGATION_LABELS[query.aggregation ?? 'raw'] ?? query.aggregation ?? '';
        parts.push(query.statistic && query.statistic !== 'average' ? `${aggregation} ${query.statistic}` : aggregation);
        break;
      }
      case 'alerts': {
        parts.push(`Alerts: ${query.allAlerts || !scope ? 'all alerts' : scope}`);
        if (query.severityFilter?.length) {
          parts.push(
            query.severityFilter
              .map((s) => ALERT_SEVERITY_OPTIONS.find((o) => o.value === s)?.label ?? String(s))
              .join(', ')
          );
        }
        const source = query.alertSource === 'warehouse' ? 'Data Warehouse' : 'Operational';
        parts.push(`${source}, ${ALERT_TIME_LABELS[query.alertTimeFilter ?? 'raised']}`);
        break;
      }
      case 'health':
        parts.push(`Health (${query.healthMode ?? 'both'}): ${scope}`);
        break;
      case 'health-tree':
        parts.push(`Health tree: ${scope}${query.unhealthyOnly ? ', unhealthy only' : ''}`);
        break;
      case 'properties':
        parts.push(`Properties: ${query.propertyNames?.length ? query.propertyNames.join(', ') : 'all'}`, scope);
        break;
      default:
        parts.push(String(query.queryType));
    }
    return parts.filter(Boolean).join(' · ');
  }

  // Values for a dashboard variable (see SCOMVariableSupport). The class or
  // group can itself be another variable.
  async metricFindQuery(query: SCOMVariableQuery, options?: LegacyMetricFindQueryOptions): Promise<MetricFindValue[]> {
    const type = query?.type ?? 'instances';
    const classId = interpolateValue(query?.class?.value, options?.scopedVars);
    const groupId = interpolateValue(query?.group?.value, options?.scopedVars);
    if (type === 'instances' && !classId && !groupId) {
      return [];
    }
    const results = await this.getResource<ResourceOption[]>('variable-values', { type, classId, groupId });
    return results.map((r) => ({ text: r.label, value: r.value }));
  }

  // The picker lookups below may be scoped by a dashboard variable, so the
  // scope is interpolated (and multi-value instance variables expanded) first.
  private scopeParams(instanceIds: string[] = [], classId?: string, groupId?: string) {
    return {
      instanceId: instanceIds.flatMap((id) => interpolateValues(id)),
      classId: interpolateValue(classId),
      groupId: interpolateValue(groupId),
    };
  }

  searchClasses(search: string): Promise<ResourceOption[]> {
    return this.getResource('classes', { search });
  }

  searchGroups(search: string): Promise<ResourceOption[]> {
    return this.getResource('groups', { search });
  }

  searchInstances(search: string, classId?: string, groupId?: string): Promise<ResourceOption[]> {
    return this.getResource('instances', { search, ...this.scopeParams([], classId, groupId) });
  }

  searchCounterObjects(search: string, instanceIds: string[], classId?: string, groupId?: string): Promise<ResourceOption[]> {
    return this.getResource('counter-objects', { search, ...this.scopeParams(instanceIds, classId, groupId) });
  }

  searchCounterNames(
    search: string,
    object: string,
    instanceIds: string[],
    classId?: string,
    groupId?: string
  ): Promise<ResourceOption[]> {
    return this.getResource('counter-names', {
      search,
      object: interpolateValue(object),
      ...this.scopeParams(instanceIds, classId, groupId),
    });
  }

  searchCounterInstances(
    search: string,
    object: string,
    counterName: string,
    instanceIds: string[],
    classId?: string,
    groupId?: string
  ): Promise<ResourceOption[]> {
    return this.getResource('counters', {
      search,
      object: interpolateValue(object),
      counterName: interpolateValue(counterName),
      ...this.scopeParams(instanceIds, classId, groupId),
    });
  }

  listProperties(classId: string): Promise<ResourceOption[]> {
    return this.getResource('properties', { classId: interpolateValue(classId) });
  }

  listResolutionStates(): Promise<ResourceOption[]> {
    return this.getResource('resolution-states', {});
  }
}
