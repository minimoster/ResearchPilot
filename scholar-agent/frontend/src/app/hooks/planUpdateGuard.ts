// A delayed response from an earlier run must never overwrite a newer run's UI.
export function createPlanUpdateGuard() {
  let generation = 0;
  return {
    advance() {
      const current = ++generation;
      return () => current === generation;
    },
  };
}

// A failed cancel may mean the plan already completed, or that it is still
// running. Reconnect to authoritative events in either case.
export async function cancelPlanWithRecovery<T>(
  request: () => Promise<T>,
  isCurrent: () => boolean,
  onCanceled: (result: T) => void,
  reconnect: () => void,
  onFailure: (error: unknown) => void,
) {
  try {
    const result = await request();
    if (isCurrent()) onCanceled(result);
  } catch (error) {
    if (!isCurrent()) return;
    reconnect();
    onFailure(error);
  }
}
