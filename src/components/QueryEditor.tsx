import React, { useState } from 'react';
import { QueryEditorProps, SelectableValue } from '@grafana/data';
import { Alert, InlineField, InlineSwitch, RadioButtonGroup, Stack } from '@grafana/ui';
import { DataSource } from '../datasource';
import { HealthMode, MyDataSourceOptions, MyQuery, ResourceRef } from '../types';
import { ClassGroupPicker } from './ClassGroupPicker';
import { InstancePicker } from './InstancePicker';
import { CounterPicker } from './CounterPicker';
import { PropertyPicker } from './PropertyPicker';
import { AlertFilters } from './AlertFilters';

type Props = QueryEditorProps<DataSource, MyQuery, MyDataSourceOptions>;

const QUERY_TYPE_OPTIONS: Array<SelectableValue<MyQuery['queryType']>> = [
  { label: 'Performance', value: 'performance' },
  { label: 'Alerts', value: 'alerts' },
  { label: 'Health', value: 'health' },
  { label: 'Properties', value: 'properties' },
  { label: 'Health Tree', value: 'health-tree' },
];

// 'Current' is a snapshot (one row per instance) — the natural fit for a
// pie/stat/table panel. 'History' is state transitions over the time range —
// the natural fit for a time series. 'Both' (the default) keeps every
// existing query and dashboard working exactly as before this option
// existed.
const HEALTH_MODE_OPTIONS: Array<SelectableValue<HealthMode>> = [
  { label: 'Current', value: 'current' },
  { label: 'History', value: 'history' },
  { label: 'Both', value: 'both' },
];

export function QueryEditor({ query, onChange, onRunQuery, datasource }: Props) {
  const [scope, setScope] = useState<'class' | 'group'>(query.group ? 'group' : 'class');

  const update = (patch: Partial<MyQuery>) => {
    onChange({ ...query, ...patch });
  };

  const updateAndRun = (patch: Partial<MyQuery>) => {
    onChange({ ...query, ...patch });
    onRunQuery();
  };

  const scopeRef: ResourceRef | undefined = scope === 'class' ? query.class : query.group;

  return (
    <Stack direction="column" gap={0.5}>
      <InlineField label="Query type" labelWidth={14}>
        <RadioButtonGroup
          options={QUERY_TYPE_OPTIONS}
          value={query.queryType}
          onChange={(v) => update({ queryType: v ?? 'performance' })}
        />
      </InlineField>

      <ClassGroupPicker
        datasource={datasource}
        scope={scope}
        value={scopeRef}
        onScopeChange={setScope}
        onChange={(value) =>
          update(
            scope === 'class'
              ? { class: value, group: undefined, instances: [], counters: [] }
              : { group: value, class: undefined, instances: [], counters: [] }
          )
        }
      />

      <InstancePicker
        datasource={datasource}
        classId={query.class?.value}
        groupId={query.group?.value}
        value={query.instances}
        onChange={(instances) => update({ instances, counters: [] })}
      />

      {query.queryType === 'performance' && (
        <CounterPicker
          datasource={datasource}
          classId={query.class?.value}
          groupId={query.group?.value}
          instanceIds={(query.instances ?? []).map((i) => i.value)}
          object={query.object}
          counterName={query.counterName}
          value={query.counters}
          aggregation={query.aggregation ?? 'hourly'}
          legendFormat={query.legendFormat}
          filterableValue={query.filterableValue}
          onObjectChange={(object) => update({ object, counterName: undefined, counters: [] })}
          onCounterNameChange={(counterName) => update({ counterName, counters: [] })}
          onChange={(counters) => update({ counters })}
          onAggregationChange={(aggregation) => update({ aggregation })}
          onLegendFormatChange={(legendFormat) => update({ legendFormat })}
          onFilterableValueChange={(filterableValue) => updateAndRun({ filterableValue })}
          onRunQuery={onRunQuery}
        />
      )}

      {query.queryType === 'health' && (
        <InlineField label="Show" labelWidth={14}>
          <RadioButtonGroup
            options={HEALTH_MODE_OPTIONS}
            value={query.healthMode ?? 'both'}
            onChange={(v) => updateAndRun({ healthMode: v ?? 'both' })}
          />
        </InlineField>
      )}

      {query.queryType === 'health-tree' && (
        <>
          {(query.instances?.length ?? 0) !== 1 && !(query.group && !query.instances?.length) && (
            <Alert
              severity="info"
              title="Select exactly one instance above, or a group with no instances to view the group's own health, to view a health tree"
            />
          )}
          <InlineField label="Unhealthy only" labelWidth={14} tooltip="Show only monitors in a Warning or Critical state, plus their parent chain">
            <InlineSwitch
              value={!!query.unhealthyOnly}
              onChange={(e) => updateAndRun({ unhealthyOnly: e.currentTarget.checked })}
            />
          </InlineField>
        </>
      )}

      {query.queryType === 'alerts' && (
        <AlertFilters
          datasource={datasource}
          allAlerts={query.allAlerts}
          severities={query.severityFilter}
          resolutionStates={query.resolutionStateFilter}
          alertSource={query.alertSource}
          onAllAlertsChange={(allAlerts) => updateAndRun({ allAlerts })}
          onSeveritiesChange={(severityFilter) => updateAndRun({ severityFilter })}
          onResolutionStatesChange={(resolutionStateFilter) => updateAndRun({ resolutionStateFilter })}
          onAlertSourceChange={(alertSource) => updateAndRun({ alertSource })}
        />
      )}

      {query.queryType === 'properties' && (
        <PropertyPicker
          datasource={datasource}
          classId={query.class?.value}
          value={query.propertyNames}
          onChange={(propertyNames) => updateAndRun({ propertyNames })}
        />
      )}
    </Stack>
  );
}
