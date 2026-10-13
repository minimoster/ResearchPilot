/** A synchronous guard also covers two Enter presses before React rerenders. */
export function createSubmissionGate() {
  let pending = false;
  return {
    begin: (): (() => void) | null => {
      if (pending) return null;
      pending = true;
      let released = false;
      return () => {
        if (released) return;
        released = true;
        pending = false;
      };
    },
  };
}

/** Remove only this request's attachments; a newer session may have its own. */
export function consumeSubmittedAttachments<T extends { id: string }>(current: T[], submittedIds: readonly string[]): T[] {
  const submitted = new Set(submittedIds);
  return current.filter((attachment) => !submitted.has(attachment.id));
}
