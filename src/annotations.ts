import { AnnotationEvent, AnnotationSupport, DataFrame } from '@grafana/data';
import { of } from 'rxjs';

import { MyQuery } from './types';

// Annotations come from Alerts and Health (history) queries: each alert, or
// each health state change, becomes a marker on the dashboard's graphs.
// Other query types produce no annotations.

const SEVERITY_COLORS: Record<string, string> = {
  Critical: 'red',
  Warning: 'orange',
  Information: 'blue',
};

// Health state names come from each monitor's operational states, so match
// the common ones by meaning.
function healthStateColor(state: string): string {
  const s = state.toLowerCase();
  if (s.includes('critical') || s.includes('error')) {
    return 'red';
  }
  if (s.includes('warning')) {
    return 'orange';
  }
  if (s.includes('healthy') || s.includes('success')) {
    return 'green';
  }
  return 'gray';
}

function column(frame: DataFrame, name: string): unknown[] {
  return frame.fields.find((f) => f.name === name)?.values ?? [];
}

function text(values: unknown[], i: number): string {
  const v = values[i];
  return v === undefined || v === null ? '' : String(v);
}

function alertEvents(frame: DataFrame): AnnotationEvent[] {
  const times = column(frame, 'time');
  const names = column(frame, 'name');
  const entities = column(frame, 'managedEntity');
  const severities = column(frame, 'severity');
  const states = column(frame, 'resolutionState');
  const descriptions = column(frame, 'description');

  const events: AnnotationEvent[] = [];
  for (let i = 0; i < frame.length; i++) {
    const entity = text(entities, i);
    const description = text(descriptions, i);
    const severity = text(severities, i);
    events.push({
      time: Number(times[i]),
      title: text(names, i) || 'SCOM alert',
      text: description ? `${entity}: ${description}` : entity,
      tags: [severity, text(states, i), entity].filter(Boolean),
      color: SEVERITY_COLORS[severity],
    });
  }
  return events;
}

function healthChangeEvents(frame: DataFrame): AnnotationEvent[] {
  const times = column(frame, 'time');
  const entities = column(frame, 'managedEntity');
  const oldStates = column(frame, 'oldHealthState');
  const newStates = column(frame, 'newHealthState');

  const events: AnnotationEvent[] = [];
  for (let i = 0; i < frame.length; i++) {
    const entity = text(entities, i);
    const newState = text(newStates, i);
    events.push({
      time: Number(times[i]),
      title: `${entity}: ${text(oldStates, i)} → ${newState}`,
      text: `${entity} changed to ${newState}`,
      tags: [newState, entity].filter(Boolean),
      color: healthStateColor(newState),
    });
  }
  return events;
}

/** Annotation events for the alert and health history frames a query returned. */
export function framesToAnnotations(frames: DataFrame[]): AnnotationEvent[] {
  return frames.flatMap((frame) => {
    switch (frame.name) {
      case 'alerts':
        return alertEvents(frame);
      case 'healthHistory':
        return healthChangeEvents(frame);
      default:
        return [];
    }
  });
}

export const annotationSupport: AnnotationSupport<MyQuery> = {
  // Alerts raised in the dashboard's time range, whatever their state.
  getDefaultQuery: () => ({ queryType: 'alerts', resolutionStateFilter: [], alertTimeFilter: 'raised' }),
  processEvents: (_annotation, frames) => of(framesToAnnotations(frames)),
};
