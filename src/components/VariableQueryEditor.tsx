import React, { useState } from 'react';
import { QueryEditorProps, SelectableValue } from '@grafana/data';
import { InlineField, RadioButtonGroup, Stack } from '@grafana/ui';
import type { DataSource } from '../datasource';
import { MyDataSourceOptions, MyQuery, SCOMVariableQuery, VariableQueryType } from '../types';
import { ClassGroupPicker } from './ClassGroupPicker';

type Props = QueryEditorProps<DataSource, MyQuery, MyDataSourceOptions, SCOMVariableQuery>;

const TYPE_OPTIONS: Array<SelectableValue<VariableQueryType>> = [
  { label: 'Instances', value: 'instances', description: 'The instances of a class or group' },
  { label: 'Groups', value: 'groups', description: 'Every SCOM group' },
  { label: 'Classes', value: 'classes', description: 'Every SCOM class' },
];

/**
 * Edits a dashboard variable listing SCOM classes, groups, or the instances
 * of a class or group (see SCOMVariableSupport). The class or group can be
 * another variable, for cascading dropdowns.
 */
export function VariableQueryEditor({ query, onChange, datasource }: Props) {
  // A brand new variable can arrive with no query object yet.
  const current: SCOMVariableQuery = query && typeof query === 'object' ? query : { refId: 'SCOMVariable' };
  const type = current.type ?? 'instances';
  const [scope, setScope] = useState<'class' | 'group'>(current.group ? 'group' : 'class');

  const update = (patch: Partial<SCOMVariableQuery>) => onChange({ ...current, type, ...patch });

  return (
    <Stack direction="column" gap={0.5}>
      <InlineField
        label="List"
        labelWidth={14}
        tooltip="Each value's text is the name and its value the SCOM id, which is what the query editor's class, group and instance pickers expect"
      >
        <RadioButtonGroup options={TYPE_OPTIONS} value={type} onChange={(v) => update({ type: v ?? 'instances' })} />
      </InlineField>
      {type === 'instances' && (
        <ClassGroupPicker
          datasource={datasource}
          scope={scope}
          value={scope === 'class' ? current.class : current.group}
          onScopeChange={setScope}
          onChange={(value) =>
            update(scope === 'class' ? { class: value, group: undefined } : { group: value, class: undefined })
          }
        />
      )}
    </Stack>
  );
}
