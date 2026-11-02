import React from 'react';
import { InlineField, Stack, Switch } from '@grafana/ui';
import { DataSourcePluginOptionsEditorProps } from '@grafana/data';
import { ConnectionSettingsEditor } from './ConnectionSettingsEditor';
import { ConnectionOptions, DEFAULT_CONNECTION, MyDataSourceOptions, MySecureJsonData } from '../types';

interface Props extends DataSourcePluginOptionsEditorProps<MyDataSourceOptions, MySecureJsonData> {}

// What the backend assumes when "same as operational database" is on and no
// warehouse database name was saved — see DefaultWarehouseDatabase in
// pkg/models/settings.go.
const DEFAULT_WAREHOUSE_DATABASE = 'OperationsManagerDW';

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

  // With "same as operational database" on, the warehouse shares the
  // Operational database's server and credentials but keeps its own
  // database name: SCOM always keeps the two in separate databases.
  const warehouseShown: ConnectionOptions = sameAsOperational
    ? { ...operational, database: warehouse.database ?? '' }
    : warehouse;

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
        <InlineField
          label="Same server as operational database"
          labelWidth={36}
          tooltip="Use the Operational database's server and credentials. The Data Warehouse database name is still set below."
        >
          <Switch
            value={sameAsOperational}
            onChange={(e) => updateJsonData({ warehouseSameAsOperational: e.currentTarget.checked })}
          />
        </InlineField>
        <ConnectionSettingsEditor
          idPrefix="dw"
          value={warehouseShown}
          password={sameAsOperational ? secureJsonData?.operationalPassword : secureJsonData?.warehousePassword}
          passwordConfigured={
            sameAsOperational ? secureJsonFields?.operationalPassword : secureJsonFields?.warehousePassword
          }
          disabled={sameAsOperational}
          databasePlaceholder={DEFAULT_WAREHOUSE_DATABASE}
          databaseTooltip={
            sameAsOperational ? `Leave blank for ${DEFAULT_WAREHOUSE_DATABASE}, the name SCOM setup uses` : undefined
          }
          onChange={(next: ConnectionOptions) =>
            updateJsonData({ warehouse: sameAsOperational ? { ...warehouse, database: next.database } : next })
          }
          onPasswordChange={(v) => updateSecret('warehousePassword', v)}
          onResetPassword={() => resetSecret('warehousePassword')}
        />
      </div>
    </Stack>
  );
}
