import React, { useCallback } from 'react';
import { ComboboxOption, InlineField, MultiCombobox } from '@grafana/ui';
import { DataSource } from '../datasource';
import { ResourceRef } from '../types';

interface Props {
  datasource: DataSource;
  classId?: string;
  groupId?: string;
  value?: ResourceRef[];
  onChange: (value: ResourceRef[]) => void;
}

export function InstancePicker({ datasource, classId, groupId, value, onChange }: Props) {
  const disabled = !classId && !groupId;

  const loadOptions = useCallback(
    async (search: string): Promise<ComboboxOption[]> => {
      const results = await datasource.searchInstances(search, classId, groupId);
      return results.map((r) => ({ label: r.label, value: r.value }));
    },
    [datasource, classId, groupId]
  );

  return (
    <InlineField label="Instances" labelWidth={14} grow disabled={disabled}>
      <MultiCombobox
        key={`${classId ?? ''}:${groupId ?? ''}`}
        options={loadOptions}
        value={(value ?? []).map((v) => ({ label: v.label, value: v.value }))}
        placeholder={disabled ? 'Choose a class or group first' : 'Search instances...'}
        onChange={(opts) =>
          onChange(
            opts.filter((o) => o.value).map((o) => ({ value: String(o.value), label: o.label ?? String(o.value) }))
          )
        }
      />
    </InlineField>
  );
}
