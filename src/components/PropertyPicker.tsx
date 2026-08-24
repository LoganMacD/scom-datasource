import React, { useCallback } from 'react';
import { ComboboxOption, InlineField, MultiCombobox } from '@grafana/ui';
import { DataSource } from '../datasource';

interface Props {
  datasource: DataSource;
  classId?: string;
  value?: string[];
  onChange: (value: string[]) => void;
}

export function PropertyPicker({ datasource, classId, value, onChange }: Props) {
  const loadOptions = useCallback(async (): Promise<ComboboxOption[]> => {
    if (!classId) {
      return [];
    }
    const results = await datasource.listProperties(classId);
    return results.map((r) => ({ label: r.label, value: r.value }));
  }, [datasource, classId]);

  return (
    <InlineField
      label="Properties"
      labelWidth={14}
      grow
      tooltip={
        !classId ? 'Searching by group: type property names manually, or pick by class for suggestions' : undefined
      }
    >
      <MultiCombobox
        key={classId ?? ''}
        options={loadOptions}
        createCustomValue={!classId}
        value={value ?? []}
        placeholder={classId ? 'All properties' : 'Type property names...'}
        onChange={(opts) => onChange(opts.filter((o) => o.value).map((o) => String(o.value)))}
      />
    </InlineField>
  );
}
