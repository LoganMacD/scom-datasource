import React, { useEffect, useState } from 'react';
import { ComboboxOption, InlineField, InlineSwitch, MultiCombobox, RadioButtonGroup } from '@grafana/ui';
import { SelectableValue } from '@grafana/data';
import type { DataSource } from '../datasource';
import { ALERT_SEVERITY_OPTIONS, AlertSource, AlertTimeFilter } from '../types';
import { loadErrorMessage } from './useLoadError';

const ALERT_SOURCE_OPTIONS: Array<SelectableValue<AlertSource>> = [
  { label: 'Operational', value: 'operational' },
  { label: 'Data Warehouse', value: 'warehouse' },
];

const TIME_FILTER_OPTIONS: Array<SelectableValue<AlertTimeFilter>> = [
  { label: 'Raised in range', value: 'raised', description: 'Alerts raised during the time range' },
  {
    label: 'Active in range',
    value: 'active',
    description: 'Alerts open at any point in the time range, however long ago they were raised',
  },
  { label: 'Ignore', value: 'ignore', description: 'Every alert, whenever it was raised' },
];

interface Props {
  datasource: DataSource;
  allAlerts?: boolean;
  severities?: number[];
  resolutionStates?: number[];
  alertSource?: AlertSource;
  timeFilter?: AlertTimeFilter;
  onAllAlertsChange: (value: boolean) => void;
  onSeveritiesChange: (value: number[]) => void;
  onResolutionStatesChange: (value: number[]) => void;
  onAlertSourceChange: (value: AlertSource) => void;
  onTimeFilterChange: (value: AlertTimeFilter) => void;
}

export function AlertFilters({
  datasource,
  allAlerts,
  severities,
  resolutionStates,
  alertSource,
  timeFilter,
  onAllAlertsChange,
  onSeveritiesChange,
  onResolutionStatesChange,
  onAlertSourceChange,
  onTimeFilterChange,
}: Props) {
  // dbo.ResolutionState holds SCOM's built-in states plus any an
  // administrator has added, so this can't be a fixed list like severity.
  const [resolutionStateOptions, setResolutionStateOptions] = useState<Array<ComboboxOption<number>>>([]);
  const [resolutionStateError, setResolutionStateError] = useState<string>();

  useEffect(() => {
    let cancelled = false;
    datasource.listResolutionStates().then(
      (results) => {
        if (!cancelled) {
          setResolutionStateOptions(results.map((r) => ({ label: r.label, value: Number(r.value) })));
          setResolutionStateError(undefined);
        }
      },
      (err: unknown) => {
        if (!cancelled) {
          setResolutionStateError(loadErrorMessage(err));
        }
      }
    );
    return () => {
      cancelled = true;
    };
  }, [datasource]);

  // The warehouse only supports alerts raised in the range (see
  // QueryAlertsWarehouse in pkg/scom/alerts.go).
  const warehouse = alertSource === 'warehouse';

  return (
    <>
      <InlineField
        label="Source"
        labelWidth={14}
        tooltip="The Data Warehouse keeps alert history far longer than the Operational database, but doesn't track alert Owner, only approximates last-modified time, and only shows alerts raised in the time range"
      >
        <RadioButtonGroup
          options={ALERT_SOURCE_OPTIONS}
          value={alertSource ?? 'operational'}
          onChange={(v) => onAlertSourceChange(v ?? 'operational')}
        />
      </InlineField>
      <InlineField
        label="Time range"
        labelWidth={14}
        disabled={warehouse}
        tooltip={
          warehouse
            ? 'The Data Warehouse source only shows alerts raised in the time range. Use the Operational source to see open alerts.'
            : 'Use "Active in range" for open alerts: an alert raised yesterday that is still open shows on a "last hour" dashboard'
        }
      >
        <RadioButtonGroup
          options={TIME_FILTER_OPTIONS}
          value={warehouse ? 'raised' : (timeFilter ?? 'raised')}
          disabled={warehouse}
          onChange={(v) => onTimeFilterChange(v ?? 'raised')}
        />
      </InlineField>
      <InlineField label="All alerts" labelWidth={14} tooltip="Ignore class/group/instance selection and return every alert">
        <InlineSwitch value={!!allAlerts} onChange={(e) => onAllAlertsChange(e.currentTarget.checked)} />
      </InlineField>
      <InlineField label="Severity" labelWidth={14}>
        <MultiCombobox
          options={ALERT_SEVERITY_OPTIONS}
          value={severities ?? []}
          placeholder="All severities"
          onChange={(opts) => onSeveritiesChange(opts.filter((o) => o.value !== undefined).map((o) => Number(o.value)))}
        />
      </InlineField>
      <InlineField
        label="Resolution state"
        labelWidth={14}
        invalid={!!resolutionStateError}
        error={resolutionStateError}
      >
        <MultiCombobox
          options={resolutionStateOptions}
          value={resolutionStates ?? []}
          placeholder="All states"
          onChange={(opts) =>
            onResolutionStatesChange(opts.filter((o) => o.value !== undefined).map((o) => Number(o.value)))
          }
        />
      </InlineField>
    </>
  );
}
