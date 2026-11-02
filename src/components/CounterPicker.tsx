import React, { ChangeEvent, useCallback } from 'react';
import { SelectableValue } from '@grafana/data';
import { Combobox, ComboboxOption, InlineField, InlineSwitch, Input, MultiCombobox, RadioButtonGroup } from '@grafana/ui';
import type { DataSource } from '../datasource';
import { Aggregation, ResourceRef, Statistic } from '../types';
import { useLoadError } from './useLoadError';

interface Props {
  datasource: DataSource;
  classId?: string;
  groupId?: string;
  instanceIds: string[];
  object?: ResourceRef;
  counterName?: ResourceRef;
  value?: ResourceRef[];
  aggregation: Aggregation;
  statistic?: Statistic;
  legendFormat?: string;
  filterableValue?: boolean;
  onObjectChange: (value?: ResourceRef) => void;
  onCounterNameChange: (value?: ResourceRef) => void;
  onChange: (value: ResourceRef[]) => void;
  onAggregationChange: (aggregation: Aggregation) => void;
  onStatisticChange: (statistic: Statistic) => void;
  onLegendFormatChange: (legendFormat: string) => void;
  onFilterableValueChange: (filterableValue: boolean) => void;
  // Legend format only takes effect on blur, not per keystroke — matching
  // how other Grafana query editors avoid re-running the query on every
  // character typed.
  onRunQuery: () => void;
}

const AGGREGATION_OPTIONS: Array<SelectableValue<Aggregation>> = [
  {
    label: 'Auto',
    value: 'auto',
    description: 'Picks raw, hourly or daily per time range, from retention and panel width',
  },
  { label: 'Raw', value: 'raw' },
  { label: 'Hourly', value: 'hourly' },
  { label: 'Daily', value: 'daily' },
];

const STATISTIC_OPTIONS: Array<SelectableValue<Statistic>> = [
  { label: 'Average', value: 'average' },
  { label: 'Min', value: 'min' },
  { label: 'Max', value: 'max', description: 'e.g. peak CPU per hour or day' },
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
  statistic,
  legendFormat,
  filterableValue,
  onObjectChange,
  onCounterNameChange,
  onChange,
  onAggregationChange,
  onStatisticChange,
  onLegendFormatChange,
  onFilterableValueChange,
  onRunQuery,
}: Props) {
  const scopeKey = `${classId ?? ''}|${groupId ?? ''}|${instanceIds.join(',')}`;
  const objectsError = useLoadError();
  const namesError = useLoadError();
  const instancesError = useLoadError();
  const trackObjects = objectsError.track;
  const trackNames = namesError.track;
  const trackInstances = instancesError.track;

  const loadObjects = useCallback(
    async (search: string): Promise<ComboboxOption[]> => {
      const results = await trackObjects(datasource.searchCounterObjects(search, instanceIds, classId, groupId));
      return results.map((r) => ({ label: r.label, value: r.value }));
    },
    [datasource, instanceIds, classId, groupId, trackObjects]
  );

  const loadCounterNames = useCallback(
    async (search: string): Promise<ComboboxOption[]> => {
      if (!object) {
        return [];
      }
      const results = await trackNames(
        datasource.searchCounterNames(search, object.value, instanceIds, classId, groupId)
      );
      return results.map((r) => ({ label: r.label, value: r.value }));
    },
    [datasource, object, instanceIds, classId, groupId, trackNames]
  );

  const loadInstances = useCallback(
    async (search: string): Promise<ComboboxOption[]> => {
      if (!object || !counterName) {
        return [];
      }
      const results = await trackInstances(
        datasource.searchCounterInstances(search, object.value, counterName.value, instanceIds, classId, groupId)
      );
      return results.map((r) => ({ label: r.label, value: r.value }));
    },
    [datasource, object, counterName, instanceIds, classId, groupId, trackInstances]
  );

  const instancesDisabled = !object || !counterName;

  return (
    <>
      <InlineField label="Object" labelWidth={14} grow invalid={!!objectsError.error} error={objectsError.error}>
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
      <InlineField
        label="Counter"
        labelWidth={14}
        grow
        disabled={!object}
        invalid={!!namesError.error}
        error={namesError.error}
      >
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
      <InlineField
        label="Instances"
        labelWidth={14}
        grow
        disabled={instancesDisabled}
        invalid={!!instancesError.error}
        error={instancesError.error}
      >
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
          onChange={(v) => onAggregationChange(v ?? 'auto')}
        />
      </InlineField>
      {aggregation !== 'raw' && (
        <InlineField
          label="Statistic"
          labelWidth={14}
          tooltip="Which statistic of hourly and daily data to plot. Raw data is single samples, so when Auto picks raw data, every statistic shows the samples."
        >
          <RadioButtonGroup
            options={STATISTIC_OPTIONS}
            value={statistic ?? 'average'}
            onChange={(v) => onStatisticChange(v ?? 'average')}
          />
        </InlineField>
      )}
      <InlineField
        label="Legend"
        labelWidth={14}
        grow
        tooltip="Customize the series legend. Available macros: {{object}}, {{counter}}, {{instance}}, {{entity}}, {{host}}. {{entity}} is the object the counter was collected against (e.g. a Processor Information instance); {{host}} is that object's top-level hosting entity (typically the computer). Dashboard variables work too. Leave blank for the default: Object - Counter [Instance] (Entity)."
      >
        <Input
          value={legendFormat ?? ''}
          placeholder="{{object}} - {{counter}} [{{instance}}] ({{entity}}) on {{host}}"
          onChange={(e: ChangeEvent<HTMLInputElement>) => onLegendFormatChange(e.target.value)}
          onBlur={onRunQuery}
        />
      </InlineField>
      <InlineField
        label="Filterable value"
        labelWidth={14}
        tooltip="Adds a second copy of the value, named 'value' in every series, so the 'Filter data by values' transformation can target it. The normal value column is named after the legend, which differs per series, so a filter can only ever match one of them. The copy is hidden from the graph, legend and tooltip — it shows up in Table view and in the transformation's field list. Leave off unless you need it: it doubles the values sent to the browser."
      >
        <InlineSwitch
          value={!!filterableValue}
          onChange={(e) => onFilterableValueChange(e.currentTarget.checked)}
        />
      </InlineField>
    </>
  );
}
