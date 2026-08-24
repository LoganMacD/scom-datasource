import React, { useEffect, useState } from 'react';
import { ComboboxOption, InlineField, InlineSwitch, MultiCombobox, RadioButtonGroup } from '@grafana/ui';
import { SelectableValue } from '@grafana/data';
import { DataSource } from '../datasource';
import { ALERT_SEVERITY_OPTIONS, AlertSource } from '../types';

const ALERT_SOURCE_OPTIONS: Array<SelectableValue<AlertSource>> = [
  { label: 'Operational', value: 'operational' },
  { label: 'Data Warehouse', value: 'warehouse' },
];

interface Props {
  datasource: DataSource;
  allAlerts?: boolean;
  severities?: number[];
  resolutionStates?: number[];
  alertSource?: AlertSource;
  onAllAlertsChange: (value: boolean) => void;
  onSeveritiesChange: (value: number[]) => void;
  onResolutionStatesChange: (value: number[]) => void;
  onAlertSourceChange: (value: AlertSource) => void;
}

export function AlertFilters({
  datasource,
  allAlerts,
  severities,
  resolutionStates,
  alertSource,
  onAllAlertsChange,
  onSeveritiesChange,
  onResolutionStatesChange,
  onAlertSourceChange,
}: Props) {
  // dbo.ResolutionState holds SCOM's built-in states plus any an
  // administrator has added, so this can't be a fixed list like severity.
  const [resolutionStateOptions, setResolutionStateOptions] = useState<Array<ComboboxOption<number>>>([]);

  useEffect(() => {
    let cancelled = false;
    datasource.listResolutionStates().then((results) => {
      if (!cancelled) {
        setResolutionStateOptions(results.map((r) => ({ label: r.label, value: Number(r.value) })));
      }
    });
    return () => {
      cancelled = true;
    };
  }, [datasource]);

  return (
    <>
      <InlineField
        label="Source"
        labelWidth={14}
        tooltip="The Data Warehouse keeps alert history far longer than the Operational database, but doesn't track alert Owner and only approximates last-modified time"
      >
        <RadioButtonGroup
          options={ALERT_SOURCE_OPTIONS}
          value={alertSource ?? 'operational'}
          onChange={(v) => onAlertSourceChange(v ?? 'operational')}
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
      <InlineField label="Resolution state" labelWidth={14}>
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
