import { test, expect } from '@grafana/plugin-e2e';

// The data source only appears in the annotation editor's picker when
// plugin.json declares "annotations": true.
test('smoke: should render the annotation query editor', async ({
  annotationEditPage,
  page,
  readProvisionedDataSource,
}) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await annotationEditPage.datasource.set(ds.name);
  await expect(page.getByTestId('scom-query-editor')).toBeVisible();
});
