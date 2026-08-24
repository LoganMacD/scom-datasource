import React, { useCallback, useState } from 'react';
import { SelectableValue } from '@grafana/data';
import { Combobox, ComboboxOption, InlineField, RadioButtonGroup } from '@grafana/ui';
import { DataSource } from '../datasource';
import { ClassSearchBy, ResourceRef } from '../types';

interface Props {
  datasource: DataSource;
  scope: 'class' | 'group';
  value?: ResourceRef;
  onScopeChange: (scope: 'class' | 'group') => void;
  onChange: (value?: ResourceRef) => void;
}

const SCOPE_OPTIONS: Array<SelectableValue<'class' | 'group'>> = [
  { label: 'Class', value: 'class' },
  { label: 'Group', value: 'group' },
];

const CLASS_SEARCH_BY_OPTIONS: Array<SelectableValue<ClassSearchBy>> = [
  { label: 'Name', value: 'name' },
  { label: 'Display name', value: 'displayName' },
];

export function ClassGroupPicker({ datasource, scope, value, onScopeChange, onChange }: Props) {
  const [classSearchBy, setClassSearchBy] = useState<ClassSearchBy>('name');

  const loadOptions = useCallback(
    async (search: string): Promise<ComboboxOption[]> => {
      const results = await (scope === 'class'
        ? datasource.searchClasses(search, classSearchBy)
        : datasource.searchGroups(search));
      return results.map((r) => ({ label: r.label, value: r.value }));
    },
    [datasource, scope, classSearchBy]
  );

  return (
    <>
      <InlineField label="Search by" labelWidth={14}>
        <RadioButtonGroup
          options={SCOPE_OPTIONS}
          value={scope}
          onChange={(v) => {
            onScopeChange(v ?? 'class');
            onChange(undefined);
          }}
        />
      </InlineField>
      {scope === 'class' && (
        <InlineField label="Match on" labelWidth={14}>
          <RadioButtonGroup
            options={CLASS_SEARCH_BY_OPTIONS}
            value={classSearchBy}
            onChange={(v) => {
              setClassSearchBy(v ?? 'name');
              onChange(undefined);
            }}
          />
        </InlineField>
      )}
      <InlineField label={scope === 'class' ? 'Class' : 'Group'} labelWidth={14} grow>
        <Combobox
          key={`${scope}:${classSearchBy}`}
          options={loadOptions}
          value={value ? { label: value.label, value: value.value } : null}
          placeholder={`Search ${scope === 'class' ? 'classes' : 'groups'}...`}
          isClearable
          onChange={(opt) =>
            onChange(opt?.value ? { value: String(opt.value), label: opt.label ?? String(opt.value) } : undefined)
          }
        />
      </InlineField>
    </>
  );
}
