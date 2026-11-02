import { test, expect } from '@grafana/plugin-e2e';

test('smoke: should render the variable query editor', async ({ variableEditPage, page, readProvisionedDataSource }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await variableEditPage.setVariableType('Query');
  await variableEditPage.datasource.set(ds.name);
  await expect(page.getByRole('radio', { name: 'Instances' })).toBeVisible();
  await expect(page.getByRole('radio', { name: 'Groups' })).toBeVisible();
  await expect(page.getByRole('radio', { name: 'Classes' })).toBeVisible();
});
