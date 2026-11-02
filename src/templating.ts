import { ScopedVars } from '@grafana/data';
import { getTemplateSrv } from '@grafana/runtime';

import { ResourceRef } from './types';

/**
 * Every value a picker value expands to. A dashboard variable on its own
 * (`$server`) expands to each value it has selected, so a multi-value
 * variable becomes several ids. Anything else is interpolated as plain text.
 * An unknown variable is left as written, so the backend can say which one
 * didn't resolve.
 */
export function interpolateValues(value: string | undefined, scopedVars?: ScopedVars): string[] {
  if (!value) {
    return [];
  }
  const templateSrv = getTemplateSrv();
  if (!templateSrv.containsTemplate(value)) {
    return [value];
  }

  let calls = 0;
  let collected: string[] = [];
  const replaced = templateSrv.replace(value, scopedVars, (v: string | string[]) => {
    calls++;
    collected = (Array.isArray(v) ? v : [v]).map(String);
    return collected.join(',');
  });
  if (calls === 1 && replaced === collected.join(',')) {
    return collected;
  }
  return [replaced];
}

/** The first value a picker value expands to (for single-value fields). */
export function interpolateValue(value: string | undefined, scopedVars?: ScopedVars): string | undefined {
  return interpolateValues(value, scopedVars)[0];
}

/** interpolateValue for a {value, label} selection. */
export function interpolateRef(ref: ResourceRef | undefined, scopedVars?: ScopedVars): ResourceRef | undefined {
  if (!ref) {
    return ref;
  }
  const value = interpolateValue(ref.value, scopedVars) ?? '';
  return value === ref.value ? ref : { value, label: ref.label === ref.value ? value : ref.label };
}

/** interpolateValues for a list of selections, expanding multi-value variables and dropping duplicates. */
export function interpolateRefs(refs: ResourceRef[] | undefined, scopedVars?: ScopedVars): ResourceRef[] | undefined {
  if (!refs) {
    return refs;
  }
  const seen = new Set<string>();
  const out: ResourceRef[] = [];
  for (const ref of refs) {
    const values = interpolateValues(ref.value, scopedVars);
    const expanded = values.length === 1 && values[0] === ref.value ? [ref] : values.map((v) => ({ value: v, label: v }));
    for (const r of expanded) {
      if (!seen.has(r.value)) {
        seen.add(r.value);
        out.push(r);
      }
    }
  }
  return out;
}

/** Dashboard variables as picker options (`$name`), filtered by search. Empty outside a dashboard. */
export function variableOptions(search = ''): Array<{ value: string; label: string; description: string }> {
  const s = search.toLowerCase();
  return getTemplateSrv()
    .getVariables()
    .map((v) => `$${v.name}`)
    .filter((name) => name.toLowerCase().includes(s))
    .map((name) => ({ value: name, label: name, description: 'Dashboard variable' }));
}
