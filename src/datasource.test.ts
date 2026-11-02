import { DataSourceInstanceSettings } from '@grafana/data';

import { DataSource } from './datasource';
import { mockTemplateSrv, mockVariables } from './testing/templateSrv';
import { MyDataSourceOptions, MyQuery } from './types';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => mockTemplateSrv,
}));

const settings = {
  id: 1,
  uid: 'scom',
  type: 'scom-datasource',
  name: 'SCOM',
  access: 'proxy',
  readOnly: false,
  jsonData: {},
  meta: {},
} as unknown as DataSourceInstanceSettings<MyDataSourceOptions>;

beforeEach(() => {
  for (const key of Object.keys(mockVariables)) {
    delete mockVariables[key];
  }
  Object.assign(mockVariables, { servers: ['id-1', 'id-2'], class: 'class-id', legendHost: 'web' });
});

describe('DataSource', () => {
  const ds = new DataSource(settings);

  it('supports variables and annotations', () => {
    expect(ds.variables).toBeDefined();
    expect(ds.annotations).toBeDefined();
  });

  describe('applyTemplateVariables', () => {
    it('expands a multi-value instance variable and interpolates the class and legend', () => {
      const query: MyQuery = {
        refId: 'A',
        queryType: 'health',
        class: { value: '$class', label: '$class' },
        instances: [{ value: '$servers', label: '$servers' }],
        legendFormat: '{{counter}} on $legendHost',
      };
      const result = ds.applyTemplateVariables(query, {});
      expect(result.class).toEqual({ value: 'class-id', label: 'class-id' });
      expect(result.instances).toEqual([
        { value: 'id-1', label: 'id-1' },
        { value: 'id-2', label: 'id-2' },
      ]);
      // Grafana variables are interpolated; the plugin's own {{macros}} are left for the backend.
      expect(result.legendFormat).toBe('{{counter}} on web');
    });

    it('leaves a query without variables unchanged', () => {
      const query: MyQuery = {
        refId: 'A',
        queryType: 'performance',
        object: { value: 'Processor', label: 'Processor' },
        counterName: { value: '% Processor Time', label: '% Processor Time' },
        counters: [{ value: '12', label: '_Total' }],
      };
      expect(ds.applyTemplateVariables(query, {})).toEqual(query);
    });
  });

  describe('getQueryDisplayText', () => {
    it('summarises a performance query', () => {
      const text = ds.getQueryDisplayText({
        refId: 'A',
        queryType: 'performance',
        class: { value: 'c', label: 'Windows Computer' },
        object: { value: 'Processor', label: 'Processor' },
        counterName: { value: '% Processor Time', label: '% Processor Time' },
        aggregation: 'hourly',
        statistic: 'max',
      });
      expect(text).toBe('Performance: Processor / % Processor Time · class Windows Computer · Hourly max');
    });

    it('summarises an alerts query', () => {
      const text = ds.getQueryDisplayText({
        refId: 'A',
        queryType: 'alerts',
        severityFilter: [2],
        alertTimeFilter: 'active',
      });
      expect(text).toBe('Alerts: all alerts · Critical · Operational, active in range');
    });
  });

  describe('filterQuery', () => {
    it('runs a health tree for exactly one instance', () => {
      expect(ds.filterQuery({ refId: 'A', queryType: 'health-tree', instances: [{ value: 'a', label: 'a' }] })).toBe(true);
      expect(ds.filterQuery({ refId: 'A', queryType: 'health-tree', instances: [] })).toBe(false);
    });
  });
});
