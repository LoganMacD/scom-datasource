// A stand-in for Grafana's template service in unit tests: just enough of
// replace/containsTemplate/getVariables for the plugin's interpolation.
export const mockVariables: Record<string, string | string[]> = {};

export const mockTemplateSrv = {
  getVariables: () => Object.keys(mockVariables).map((name) => ({ name })),
  containsTemplate: (target?: string) => !!target && /\$(\w+|\{\w+\})/.test(target),
  replace: (target = '', _scopedVars?: unknown, format?: string | Function) =>
    target.replace(/\$\{(\w+)\}|\$(\w+)/g, (match: string, braced?: string, plain?: string) => {
      const name = braced ?? plain ?? '';
      if (!(name in mockVariables)) {
        return match;
      }
      const value = mockVariables[name];
      if (typeof format === 'function') {
        return format(value);
      }
      return Array.isArray(value) ? `{${value.join(',')}}` : value;
    }),
};
