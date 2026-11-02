import { mockTemplateSrv, mockVariables } from './testing/templateSrv';
import { interpolateRefs, interpolateValue, interpolateValues, variableOptions } from './templating';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => mockTemplateSrv,
}));

beforeEach(() => {
  for (const key of Object.keys(mockVariables)) {
    delete mockVariables[key];
  }
  Object.assign(mockVariables, { servers: ['id-1', 'id-2'], server: 'id-3' });
});

describe('interpolateValues', () => {
  it('leaves plain values alone', () => {
    expect(interpolateValues('6f3d5b8a-1c2e-4a7b-9d10-ab12cd34ef56')).toEqual([
      '6f3d5b8a-1c2e-4a7b-9d10-ab12cd34ef56',
    ]);
  });

  it('expands a multi-value variable into one value each', () => {
    expect(interpolateValues('$servers')).toEqual(['id-1', 'id-2']);
    expect(interpolateValues('${servers}')).toEqual(['id-1', 'id-2']);
  });

  it('resolves a single-value variable', () => {
    expect(interpolateValue('$server')).toBe('id-3');
  });

  it('leaves an unknown variable as written, so the backend can name it', () => {
    expect(interpolateValues('$missing')).toEqual(['$missing']);
  });

  it('treats a variable inside other text as plain text', () => {
    expect(interpolateValues('prefix-$server')).toEqual(['prefix-id-3']);
  });

  it('returns nothing for an empty value', () => {
    expect(interpolateValues(undefined)).toEqual([]);
    expect(interpolateValues('')).toEqual([]);
  });
});

describe('interpolateRefs', () => {
  it('expands variables and drops duplicates, keeping plain selections as they are', () => {
    const refs = interpolateRefs([
      { value: '$servers', label: '$servers' },
      { value: 'id-1', label: 'server one' },
      { value: 'id-9', label: 'server nine' },
    ]);
    expect(refs).toEqual([
      { value: 'id-1', label: 'id-1' },
      { value: 'id-2', label: 'id-2' },
      { value: 'id-9', label: 'server nine' },
    ]);
  });
});

describe('variableOptions', () => {
  it('lists dashboard variables matching the search', () => {
    expect(variableOptions('').map((o) => o.value)).toEqual(['$servers', '$server']);
    expect(variableOptions('SERVERS').map((o) => o.value)).toEqual(['$servers']);
  });
});
