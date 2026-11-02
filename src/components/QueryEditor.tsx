import React, { useState } from 'react';
import { QueryEditorProps, SelectableValue } from '@grafana/data';
import { Alert, InlineField, InlineSwitch, RadioButtonGroup, Stack } from '@grafana/ui';
import type { DataSource } from '../datasource';
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
// pie/stat/table panel. 'History' is state transitions over the time range.
// 'Timeline' is each instance's state over the time range, for a State
// timeline panel. 'Both' (the default) keeps every existing query and
// dashboard working exactly as before these options existed.
const HEALTH_MODE_OPTIONS: Array<SelectableValue<HealthMode>> = [
  { label: 'Current', value: 'current' },
  { label: 'History', value: 'history' },
  { label: 'Timeline', value: 'timeline', description: 'Each instance’s state over the time range, for a State timeline panel' },
  { label: 'Both', value: 'both', description: 'Current and History' },
];

export function QueryEditor({ query, onChange, onRunQuery, datasource }: Props) {
  const [scope, setScope] = useState<'class' | 'group'>(query.group ? 'group' : 'class');

  // Every selection runs the query straight away, as Grafana's own editors
  // do; Grafana skips queries that aren't runnable yet (see filterQuery in
  // datasource.ts). Free text (the legend) runs on blur instead.
  const update = (patch: Partial<MyQuery>) => {
    onChange({ ...query, ...patch });
  };
  const updateAndRun = (patch: Partial<MyQuery>) => {
    onChange({ ...query, ...patch });
    onRunQuery();
  };

  const scopeRef: ResourceRef | undefined = scope === 'class' ? query.class : query.group;
  const coloredTypes = query.queryType === 'alerts' || (query.queryType === 'health' && query.healthMode !== 'timeline');

  return (
    <div data-testid="scom-query-editor">
      <Stack direction="column" gap={0.5}>
        <InlineField label="Query type" labelWidth={14}>
          <RadioButtonGroup
            options={QUERY_TYPE_OPTIONS}
            value={query.queryType}
            onChange={(v) => updateAndRun({ queryType: v ?? 'performance' })}
          />
        </InlineField>

        <ClassGroupPicker
          datasource={datasource}
          scope={scope}
          value={scopeRef}
          onScopeChange={setScope}
          onChange={(value) =>
            updateAndRun(
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
          onChange={(instances) => updateAndRun({ instances, counters: [] })}
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
            // Unset runs as raw on the backend, so show it as raw rather than
            // implying a grain the query isn't using. Not written back to the query.
            aggregation={query.aggregation ?? 'raw'}
            statistic={query.statistic}
            legendFormat={query.legendFormat}
            filterableValue={query.filterableValue}
            onObjectChange={(object) => updateAndRun({ object, counterName: undefined, counters: [] })}
            onCounterNameChange={(counterName) => updateAndRun({ counterName, counters: [] })}
            onChange={(counters) => updateAndRun({ counters })}
            onAggregationChange={(aggregation) => updateAndRun({ aggregation })}
            onStatisticChange={(statistic) => updateAndRun({ statistic })}
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
            <InlineField
              label="Unhealthy only"
              labelWidth={14}
              tooltip="Show only monitors in a Warning or Critical state, plus their parent chain"
            >
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
            timeFilter={query.alertTimeFilter}
            onAllAlertsChange={(allAlerts) => updateAndRun({ allAlerts })}
            onSeveritiesChange={(severityFilter) => updateAndRun({ severityFilter })}
            onResolutionStatesChange={(resolutionStateFilter) => updateAndRun({ resolutionStateFilter })}
            onAlertSourceChange={(alertSource) => updateAndRun({ alertSource })}
            onTimeFilterChange={(alertTimeFilter) => updateAndRun({ alertTimeFilter })}
          />
        )}

        {coloredTypes && (
          <InlineField
            label="Colors"
            labelWidth={14}
            tooltip="Color health states, alert severities and priorities (and show maintenance mode as Yes/No) in tables, stat panels and state timelines. Turn off to use the panel's own value mappings: the data source's take priority over them."
          >
            <InlineSwitch
              value={!!query.stateColors}
              onChange={(e) => updateAndRun({ stateColors: e.currentTarget.checked })}
            />
          </InlineField>
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
    </div>
  );
}
