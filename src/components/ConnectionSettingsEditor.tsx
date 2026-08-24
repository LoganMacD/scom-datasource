import React, { ChangeEvent } from 'react';
import { InlineField, Input, RadioButtonGroup, SecretInput, Stack, Switch } from '@grafana/ui';
import { AuthType, ConnectionOptions } from '../types';

const AUTH_OPTIONS: Array<{ label: string; value: AuthType }> = [
  { label: 'SQL login', value: 'sql' },
  { label: 'Windows / NTLM', value: 'ntlm' },
  { label: 'Kerberos', value: 'kerberos' },
];

interface Props {
  idPrefix: string;
  value: ConnectionOptions;
  password?: string;
  passwordConfigured?: boolean;
  disabled?: boolean;
  onChange: (value: ConnectionOptions) => void;
  onPasswordChange: (password: string) => void;
  onResetPassword: () => void;
}

export function ConnectionSettingsEditor({
  idPrefix,
  value,
  password,
  passwordConfigured,
  disabled,
  onChange,
  onPasswordChange,
  onResetPassword,
}: Props) {
  const set = (patch: Partial<ConnectionOptions>) => onChange({ ...value, ...patch });

  return (
    <Stack direction="column" gap={1}>
      <InlineField label="Host" labelWidth={20} disabled={disabled}>
        <Input
          id={`${idPrefix}-host`}
          value={value.host}
          placeholder="scom-sql.example.com"
          width={40}
          onChange={(e: ChangeEvent<HTMLInputElement>) => set({ host: e.target.value })}
        />
      </InlineField>
      <InlineField label="Port" labelWidth={20} disabled={disabled}>
        <Input
          id={`${idPrefix}-port`}
          value={value.port}
          placeholder="1433"
          width={40}
          onChange={(e: ChangeEvent<HTMLInputElement>) => set({ port: e.target.value })}
        />
      </InlineField>
      <InlineField
        label="Instance name"
        labelWidth={20}
        disabled={disabled}
        tooltip="Named instance, e.g. SCOM. Leave blank for the default instance. If Port is also set, it's used directly instead of resolving the instance's dynamic port via the SQL Browser service (UDP 1434), which many firewalls block."
      >
        <Input
          id={`${idPrefix}-instance`}
          value={value.instanceName ?? ''}
          placeholder="(default instance)"
          width={40}
          onChange={(e: ChangeEvent<HTMLInputElement>) => set({ instanceName: e.target.value })}
        />
      </InlineField>
      <InlineField label="Database" labelWidth={20} disabled={disabled}>
        <Input
          id={`${idPrefix}-database`}
          value={value.database}
          placeholder="OperationsManager"
          width={40}
          onChange={(e: ChangeEvent<HTMLInputElement>) => set({ database: e.target.value })}
        />
      </InlineField>
      <InlineField label="Authentication" labelWidth={20} disabled={disabled}>
        <RadioButtonGroup
          options={AUTH_OPTIONS}
          value={value.authType}
          onChange={(authType) => set({ authType: authType ?? 'sql' })}
          disabled={disabled}
        />
      </InlineField>
      <InlineField label="Username" labelWidth={20} disabled={disabled}>
        <Input
          id={`${idPrefix}-username`}
          value={value.username ?? ''}
          width={40}
          onChange={(e: ChangeEvent<HTMLInputElement>) => set({ username: e.target.value })}
        />
      </InlineField>
      <InlineField
        label="Password"
        labelWidth={20}
        interactive
        disabled={disabled}
        tooltip="Stored encrypted; never sent back to the browser"
      >
        <SecretInput
          id={`${idPrefix}-password`}
          isConfigured={!!passwordConfigured}
          value={password}
          width={40}
          onReset={onResetPassword}
          onChange={(e: ChangeEvent<HTMLInputElement>) => onPasswordChange(e.target.value)}
        />
      </InlineField>
      {value.authType === 'ntlm' && (
        <InlineField label="Domain" labelWidth={20} disabled={disabled} tooltip="NTLM domain, e.g. CONTOSO">
          <Input
            id={`${idPrefix}-domain`}
            value={value.domain ?? ''}
            width={40}
            onChange={(e: ChangeEvent<HTMLInputElement>) => set({ domain: e.target.value })}
          />
        </InlineField>
      )}
      {value.authType === 'kerberos' && (
        <InlineField label="Realm" labelWidth={20} disabled={disabled} tooltip="Kerberos realm, e.g. CONTOSO.COM">
          <Input
            id={`${idPrefix}-realm`}
            value={value.realm ?? ''}
            width={40}
            onChange={(e: ChangeEvent<HTMLInputElement>) => set({ realm: e.target.value })}
          />
        </InlineField>
      )}
      <InlineField label="Encrypt" labelWidth={20} disabled={disabled} tooltip="SQL Server connection encryption mode">
        <RadioButtonGroup
          options={[
            { label: 'Required', value: 'true' },
            { label: 'Strict', value: 'strict' },
            { label: 'Off', value: 'disable' },
          ]}
          value={value.encrypt}
          onChange={(encrypt) => set({ encrypt: encrypt ?? 'true' })}
          disabled={disabled}
        />
      </InlineField>
      <InlineField
        label="Trust server certificate"
        labelWidth={20}
        disabled={disabled}
        tooltip="Skip certificate validation; only use for trusted internal networks"
      >
        <Switch
          value={value.trustServerCertificate}
          disabled={disabled}
          onChange={(e) => set({ trustServerCertificate: e.currentTarget.checked })}
        />
      </InlineField>
    </Stack>
  );
}
