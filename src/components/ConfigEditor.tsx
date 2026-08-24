import React from 'react';
import { InlineField, Stack, Switch } from '@grafana/ui';
import { DataSourcePluginOptionsEditorProps } from '@grafana/data';
import { ConnectionSettingsEditor } from './ConnectionSettingsEditor';
import { ConnectionOptions, DEFAULT_CONNECTION, MyDataSourceOptions, MySecureJsonData } from '../types';

interface Props extends DataSourcePluginOptionsEditorProps<MyDataSourceOptions, MySecureJsonData> {}

export function ConfigEditor({ options, onOptionsChange }: Props) {
  const { jsonData, secureJsonFields, secureJsonData } = options;
  const operational = jsonData.operational ?? DEFAULT_CONNECTION;
  const warehouse = jsonData.warehouse ?? DEFAULT_CONNECTION;
  const sameAsOperational = jsonData.warehouseSameAsOperational ?? false;

  const updateJsonData = (patch: Partial<MyDataSourceOptions>) => {
    onOptionsChange({ ...options, jsonData: { ...jsonData, ...patch } });
  };

  const updateSecret = (key: keyof MySecureJsonData, value: string) => {
    onOptionsChange({ ...options, secureJsonData: { ...secureJsonData, [key]: value } });
  };

  const resetSecret = (key: keyof MySecureJsonData) => {
    onOptionsChange({
      ...options,
      secureJsonFields: { ...options.secureJsonFields, [key]: false },
      secureJsonData: { ...secureJsonData, [key]: '' },
    });
  };

  return (
    <Stack direction="column" gap={3}>
      <div>
        <h4>Operational database</h4>
        <ConnectionSettingsEditor
          idPrefix="op"
          value={operational}
          password={secureJsonData?.operationalPassword}
          passwordConfigured={secureJsonFields?.operationalPassword}
          onChange={(operational: ConnectionOptions) => updateJsonData({ operational })}
          onPasswordChange={(v) => updateSecret('operationalPassword', v)}
          onResetPassword={() => resetSecret('operationalPassword')}
        />
      </div>

      <div>
        <h4>Data Warehouse</h4>
        <InlineField label="Same as operational database" labelWidth={30}>
          <Switch
            value={sameAsOperational}
            onChange={(e) => updateJsonData({ warehouseSameAsOperational: e.currentTarget.checked })}
          />
        </InlineField>
        <ConnectionSettingsEditor
          idPrefix="dw"
          value={sameAsOperational ? operational : warehouse}
          password={sameAsOperational ? secureJsonData?.operationalPassword : secureJsonData?.warehousePassword}
          passwordConfigured={
            sameAsOperational ? secureJsonFields?.operationalPassword : secureJsonFields?.warehousePassword
          }
          disabled={sameAsOperational}
          onChange={(warehouse: ConnectionOptions) => updateJsonData({ warehouse })}
          onPasswordChange={(v) => updateSecret('warehousePassword', v)}
          onResetPassword={() => resetSecret('warehousePassword')}
        />
      </div>
    </Stack>
  );
}
