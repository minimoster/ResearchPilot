/** A stream owns its callbacks and deduplicates durable replay across reconnects. */
export interface PlanStreamEvent {
  plan_id: string;
}

export function subscribeToPlanEvents<T extends PlanStreamEvent>(
  source: EventSource,
  planId: string,
  handlers: { onPlanEvent: (event: T) => void; onError?: () => void },
): EventSource {
  const seen = new Set<string>();
  let closed = false;
  const close = source.close.bind(source);
  source.close = () => {
    closed = true;
    close();
  };
  source.addEventListener('plan_event', (message) => {
    if (closed) return;
    const raw = (message as MessageEvent).data;
    let event: T;
    try {
      event = JSON.parse(raw) as T;
    } catch {
      return;
    }
    if (!event || event.plan_id !== planId) return;
    const key = (message as MessageEvent).lastEventId || raw;
    if (seen.has(key)) return;
    seen.add(key);
    handlers.onPlanEvent(event);
  });
  source.onerror = () => {
    // CONNECTING means the browser is already retrying. Calling close() here
    // disables that recovery and leaves the backend execution unobserved.
    if (!closed && source.readyState === 2) {
      source.close();
      handlers.onError?.();
    }
  };
  return source;
}

/** Invalidate async results when the user switches session or plan. */
export function createExecutionScope() {
  let generation = 0;
  return {
    invalidate: () => { generation += 1; },
    capture: () => {
      const current = generation;
      return () => current === generation;
    },
  };
}
