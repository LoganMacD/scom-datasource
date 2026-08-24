import { test, expect } from '@grafana/plugin-e2e';

test('smoke: should render config editor', async ({ createDataSourceConfigPage, readProvisionedDataSource, page }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await createDataSourceConfigPage({ type: ds.type });
  await expect(page.getByRole('textbox', { name: 'Host' }).first()).toBeVisible();
});

test('"Save & test" should fail when the operational database is unreachable', async ({
  createDataSourceConfigPage,
  readProvisionedDataSource,
  page,
}) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  const configPage = await createDataSourceConfigPage({ type: ds.type });
  await page.getByRole('textbox', { name: 'Host' }).first().fill('unreachable-host.invalid');
  await expect(configPage.saveAndTest()).not.toBeOK();
});
