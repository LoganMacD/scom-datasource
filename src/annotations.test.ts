import { FieldType, createDataFrame } from '@grafana/data';
import { firstValueFrom } from 'rxjs';

import { annotationSupport, framesToAnnotations } from './annotations';

const t0 = Date.UTC(2026, 0, 1);

const alerts = createDataFrame({
  name: 'alerts',
  fields: [
    { name: 'time', type: FieldType.time, values: [t0] },
    { name: 'alertId', type: FieldType.string, values: ['a-1'] },
    { name: 'name', type: FieldType.string, values: ['Logical disk free space is low'] },
    { name: 'managedEntity', type: FieldType.string, values: ['C:'] },
    { name: 'severity', type: FieldType.string, values: ['Critical'] },
    { name: 'resolutionState', type: FieldType.string, values: ['New'] },
    { name: 'description', type: FieldType.string, values: ['Only 2% free'] },
  ],
});

const history = createDataFrame({
  name: 'healthHistory',
  fields: [
    { name: 'time', type: FieldType.time, values: [t0 + 1000] },
    { name: 'managedEntity', type: FieldType.string, values: ['server01'] },
    { name: 'oldHealthState', type: FieldType.string, values: ['Healthy'] },
    { name: 'newHealthState', type: FieldType.string, values: ['Warning'] },
  ],
});

describe('framesToAnnotations', () => {
  it('turns each alert into an annotation colored by severity', () => {
    const [event] = framesToAnnotations([alerts]);
    expect(event).toMatchObject({
      time: t0,
      title: 'Logical disk free space is low',
      text: 'C:: Only 2% free',
      tags: ['Critical', 'New', 'C:'],
      color: 'red',
    });
  });

  it('turns each health change into an annotation', () => {
    const [event] = framesToAnnotations([history]);
    expect(event).toMatchObject({
      time: t0 + 1000,
      title: 'server01: Healthy → Warning',
      tags: ['Warning', 'server01'],
      color: 'orange',
    });
  });

  it('ignores frames other query types return', () => {
    const perf = createDataFrame({ name: 'Processor - % Processor Time (server01)', fields: [] });
    expect(framesToAnnotations([perf])).toEqual([]);
  });

  it('is what the annotation support uses', async () => {
    const events = await firstValueFrom(annotationSupport.processEvents!({} as never, [alerts, history]));
    expect(events).toHaveLength(2);
  });

  it('defaults to alerts raised in the time range, in any state', () => {
    expect(annotationSupport.getDefaultQuery!()).toEqual({
      queryType: 'alerts',
      resolutionStateFilter: [],
      alertTimeFilter: 'raised',
    });
  });
});
