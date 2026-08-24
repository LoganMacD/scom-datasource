import { DataSourceInstanceSettings, CoreApp, ScopedVars } from '@grafana/data';
import { DataSourceWithBackend, getTemplateSrv } from '@grafana/runtime';

import { MyQuery, MyDataSourceOptions, DEFAULT_QUERY, ClassSearchBy } from './types';

export interface ResourceOption {
  value: string;
  label: string;
}

export class DataSource extends DataSourceWithBackend<MyQuery, MyDataSourceOptions> {
  constructor(instanceSettings: DataSourceInstanceSettings<MyDataSourceOptions>) {
    super(instanceSettings);
  }

  getDefaultQuery(_: CoreApp): Partial<MyQuery> {
    return DEFAULT_QUERY;
  }

  applyTemplateVariables(query: MyQuery, scopedVars: ScopedVars) {
    const templateSrv = getTemplateSrv();
    return {
      ...query,
      instances: query.instances?.map((i) => ({ ...i, value: templateSrv.replace(i.value, scopedVars) })),
      counters: query.counters?.map((c) => ({ ...c, value: templateSrv.replace(c.value, scopedVars) })),
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
      default:
        return false;
    }
  }

  searchClasses(search: string, by: ClassSearchBy = 'name'): Promise<ResourceOption[]> {
    return this.getResource('classes', { search, by });
  }

  searchGroups(search: string): Promise<ResourceOption[]> {
    return this.getResource('groups', { search });
  }

  searchInstances(search: string, classId?: string, groupId?: string): Promise<ResourceOption[]> {
    return this.getResource('instances', { search, classId, groupId });
  }

  searchCounterObjects(search: string, instanceIds: string[], classId?: string, groupId?: string): Promise<ResourceOption[]> {
    return this.getResource('counter-objects', { search, instanceId: instanceIds, classId, groupId });
  }

  searchCounterNames(
    search: string,
    object: string,
    instanceIds: string[],
    classId?: string,
    groupId?: string
  ): Promise<ResourceOption[]> {
    return this.getResource('counter-names', { search, object, instanceId: instanceIds, classId, groupId });
  }

  searchCounterInstances(
    search: string,
    object: string,
    counterName: string,
    instanceIds: string[],
    classId?: string,
    groupId?: string
  ): Promise<ResourceOption[]> {
    return this.getResource('counters', { search, object, counterName, instanceId: instanceIds, classId, groupId });
  }

  listProperties(classId: string): Promise<ResourceOption[]> {
    return this.getResource('properties', { classId });
  }

  listResolutionStates(): Promise<ResourceOption[]> {
    return this.getResource('resolution-states', {});
  }
}
