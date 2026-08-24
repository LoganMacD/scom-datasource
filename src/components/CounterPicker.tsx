import React, { useCallback } from 'react';
import { SelectableValue } from '@grafana/data';
import { Combobox, ComboboxOption, InlineField, MultiCombobox, RadioButtonGroup } from '@grafana/ui';
import { DataSource } from '../datasource';
import { Aggregation, ResourceRef } from '../types';

interface Props {
  datasource: DataSource;
  classId?: string;
  groupId?: string;
  instanceIds: string[];
  object?: ResourceRef;
  counterName?: ResourceRef;
  value?: ResourceRef[];
  aggregation: Aggregation;
  onObjectChange: (value?: ResourceRef) => void;
  onCounterNameChange: (value?: ResourceRef) => void;
  onChange: (value: ResourceRef[]) => void;
  onAggregationChange: (aggregation: Aggregation) => void;
}

const AGGREGATION_OPTIONS: Array<SelectableValue<Aggregation>> = [
  { label: 'Raw', value: 'raw' },
  { label: 'Hourly', value: 'hourly' },
  { label: 'Daily', value: 'daily' },
];

export function CounterPicker({
  datasource,
  classId,
  groupId,
  instanceIds,
  object,
  counterName,
  value,
  aggregation,
  onObjectChange,
  onCounterNameChange,
  onChange,
  onAggregationChange,
}: Props) {
  const scopeKey = `${classId ?? ''}|${groupId ?? ''}|${instanceIds.join(',')}`;

  const loadObjects = useCallback(
    async (search: string): Promise<ComboboxOption[]> => {
      const results = await datasource.searchCounterObjects(search, instanceIds, classId, groupId);
      return results.map((r) => ({ label: r.label, value: r.value }));
    },
    [datasource, instanceIds, classId, groupId]
  );

  const loadCounterNames = useCallback(
    async (search: string): Promise<ComboboxOption[]> => {
      if (!object) {
        return [];
      }
      const results = await datasource.searchCounterNames(search, object.value, instanceIds, classId, groupId);
      return results.map((r) => ({ label: r.label, value: r.value }));
    },
    [datasource, object, instanceIds, classId, groupId]
  );

  const loadInstances = useCallback(
    async (search: string): Promise<ComboboxOption[]> => {
      if (!object || !counterName) {
        return [];
      }
      const results = await datasource.searchCounterInstances(
        search,
        object.value,
        counterName.value,
        instanceIds,
        classId,
        groupId
      );
      return results.map((r) => ({ label: r.label, value: r.value }));
    },
    [datasource, object, counterName, instanceIds, classId, groupId]
  );

  const instancesDisabled = !object || !counterName;

  return (
    <>
      <InlineField label="Object" labelWidth={14} grow>
        <Combobox
          key={scopeKey}
          options={loadObjects}
          value={object ? { label: object.label, value: object.value } : null}
          placeholder="Search performance objects..."
          isClearable
          onChange={(opt) =>
            onObjectChange(
              opt?.value ? { value: String(opt.value), label: opt.label ?? String(opt.value) } : undefined
            )
          }
        />
      </InlineField>
      <InlineField label="Counter" labelWidth={14} grow disabled={!object}>
        <Combobox
          key={`${scopeKey}|${object?.value ?? ''}`}
          options={loadCounterNames}
          value={counterName ? { label: counterName.label, value: counterName.value } : null}
          placeholder={object ? 'Search counters...' : 'Choose an object first'}
          isClearable
          onChange={(opt) =>
            onCounterNameChange(
              opt?.value ? { value: String(opt.value), label: opt.label ?? String(opt.value) } : undefined
            )
          }
        />
      </InlineField>
      <InlineField label="Instances" labelWidth={14} grow disabled={instancesDisabled}>
        <MultiCombobox
          key={`${scopeKey}|${object?.value ?? ''}|${counterName?.value ?? ''}`}
          options={loadInstances}
          value={(value ?? []).map((v) => ({ label: v.label, value: v.value }))}
          placeholder={instancesDisabled ? 'Choose an object and counter first' : 'All instances (search to narrow)'}
          onChange={(opts) =>
            onChange(
              opts.filter((o) => o.value).map((o) => ({ value: String(o.value), label: o.label ?? String(o.value) }))
            )
          }
        />
      </InlineField>
      <InlineField label="Aggregation" labelWidth={14}>
        <RadioButtonGroup
          options={AGGREGATION_OPTIONS}
          value={aggregation}
          onChange={(v) => onAggregationChange(v ?? 'hourly')}
        />
      </InlineField>
    </>
  );
}
