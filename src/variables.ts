import { CustomVariableSupport, DataQueryRequest, DataQueryResponse } from '@grafana/data';
import { from, Observable } from 'rxjs';
import { map } from 'rxjs/operators';

import type { DataSource } from './datasource';
import { VariableQueryEditor } from './components/VariableQueryEditor';
import { SCOMVariableQuery } from './types';

/**
 * Dashboard variables listing SCOM classes, groups, or the instances of a
 * class or group. Each value's text is the name and its value the SCOM id,
 * so `$server` can go straight into the query editor's instance picker.
 */
export class SCOMVariableSupport extends CustomVariableSupport<DataSource, SCOMVariableQuery> {
  editor = VariableQueryEditor;

  constructor(private readonly datasource: DataSource) {
    super();
  }

  query(request: DataQueryRequest<SCOMVariableQuery>): Observable<DataQueryResponse> {
    const [query] = request.targets;
    return from(this.datasource.metricFindQuery(query, { scopedVars: request.scopedVars, range: request.range })).pipe(
      map((data) => ({ data }))
    );
  }
}
