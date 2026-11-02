import React, { useCallback } from 'react';
import { SelectableValue } from '@grafana/data';
import { Combobox, ComboboxOption, InlineField, RadioButtonGroup } from '@grafana/ui';
import type { DataSource } from '../datasource';
import { variableOptions } from '../templating';
import { ResourceRef } from '../types';
import { useLoadError } from './useLoadError';

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

export function ClassGroupPicker({ datasource, scope, value, onScopeChange, onChange }: Props) {
  const { error, track } = useLoadError();

  // One search box: a class matches on its display name ("Windows Computer")
  // or its technical name ("Microsoft.Windows.Computer"), which is shown
  // underneath. Dashboard variables are offered first.
  const loadOptions = useCallback(
    async (search: string): Promise<ComboboxOption[]> => {
      const results = await track(scope === 'class' ? datasource.searchClasses(search) : datasource.searchGroups(search));
      return [
        ...variableOptions(search),
        ...results.map((r) => ({ label: r.label, value: r.value, description: r.description })),
      ];
    },
    [datasource, scope, track]
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
      <InlineField
        label={scope === 'class' ? 'Class' : 'Group'}
        labelWidth={14}
        grow
        invalid={!!error}
        error={error}
        tooltip={
          scope === 'class'
            ? 'Search by display name (e.g. Windows Computer) or technical name (e.g. Microsoft.Windows.Computer). Use * and ? as wildcards.'
            : 'Use * and ? as wildcards.'
        }
      >
        <Combobox
          key={scope}
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
