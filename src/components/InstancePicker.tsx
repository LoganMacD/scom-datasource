import React, { useCallback } from 'react';
import { ComboboxOption, InlineField, MultiCombobox } from '@grafana/ui';
import type { DataSource } from '../datasource';
import { variableOptions } from '../templating';
import { ResourceRef } from '../types';
import { useLoadError } from './useLoadError';

interface Props {
  datasource: DataSource;
  classId?: string;
  groupId?: string;
  value?: ResourceRef[];
  onChange: (value: ResourceRef[]) => void;
}

export function InstancePicker({ datasource, classId, groupId, value, onChange }: Props) {
  const disabled = !classId && !groupId;
  const { error, track } = useLoadError();

  // Dashboard variables are offered first, so a dashboard can have one
  // $server dropdown driving every panel.
  const loadOptions = useCallback(
    async (search: string): Promise<ComboboxOption[]> => {
      const results = await track(datasource.searchInstances(search, classId, groupId));
      return [...variableOptions(search), ...results.map((r) => ({ label: r.label, value: r.value }))];
    },
    [datasource, classId, groupId, track]
  );

  return (
    <InlineField
      label="Instances"
      labelWidth={14}
      grow
      disabled={disabled}
      invalid={!!error}
      error={error}
      tooltip="Leave empty for every instance in the class or group. A multi-value dashboard variable picks several."
    >
      <MultiCombobox
        key={`${classId ?? ''}:${groupId ?? ''}`}
        options={loadOptions}
        value={(value ?? []).map((v) => ({ label: v.label, value: v.value }))}
        placeholder={disabled ? 'Choose a class or group first' : 'All instances (search to narrow)'}
        onChange={(opts) =>
          onChange(
            opts.filter((o) => o.value).map((o) => ({ value: String(o.value), label: o.label ?? String(o.value) }))
          )
        }
      />
    </InlineField>
  );
}
