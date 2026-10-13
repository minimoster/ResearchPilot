# Sandbox service security and lifecycle notes

- Without `SANDBOX_API_TOKEN`, the standalone service binds to
  `127.0.0.1:8082`. Configuring a nonblank token preserves the existing `:8082`
  listener; execution and lifecycle requests must carry that bearer token.
- The standalone and embedded Docker engines add the persistent label
  `org.openresearch.sandbox.managed=true` when creating containers. Before
  executing or deleting a container, the service inspects this label. A fresh
  service process can recognize labeled containers without an in-memory map.
- **Upgrade behavior:** containers created by older versions without this label
  are deliberately not adopted. Recreate these sandboxes after upgrading. Their
  execution and deletion requests will fail with an explicit ownership error;
  the service does not automatically relabel or delete legacy containers.
- OpenSandbox fallback does not support local host workspace mounts. Requests
  requiring a mount fail explicitly rather than silently losing workspace data.
- Generated images must be regular files confined to the workspace, at most
  8 MiB. Streaming execution bounds each captured output stream to 8 MiB. The
  backend accepts bounded final JSON events up to 128 MiB to allow JSON escaping
  and base64 image expansion.
- Cancellation reaps the local Docker CLI and drains its readers, but does not
  yet guarantee termination of the daemon-side execution process. Do not treat
  a canceled request as proof that code inside the container has stopped.
