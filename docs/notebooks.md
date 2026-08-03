# Mojojojo Notebooks

Mojojojo Notebooks is the hosted product built around the reusable
`mojo-notebook` runtime. Local notebooks remain plain Markdown and work without
an account; mojojojo adds cloud concerns around them.

## Ownership boundary

| Layer | Owns |
| --- | --- |
| `mojo-notebook` | Cells, dependency graph, local execution, rendering, Marimo import, plugin contracts |
| notebook service | Revisions, sharing, sessions, collaboration, quotas, audit events |
| existing mojojojo platform | Identity, credits, placement, sandboxing, compiled Mojo cache |
| agent service | Model calls and streaming with scoped context, never credentials or a kernel handle |

Notebook execution must use the existing planner and runner. Do not create a
second compiler, sandbox, billing path, agent proxy, or user database.

## Plugin model

The runtime registry has three contribution points:

- **importers** turn an external format into cells; the Marimo importer is the reference;
- **renderers** turn typed output into safe HTML or another artifact;
- **actions** perform an explicit user command and return a message, artifacts, and proposed edits.

The host constructs the registry from an allowlist. Plugins declare capabilities
such as `network`, `datasets:read`, `artifacts:write`, or `models:invoke`;
registration fails when a capability is not granted. Discovery is never automatic
in hosted workers, names cannot collide, and plugin versions are recorded with
every action and output.

Plugins do not receive the live kernel, database handles, account secrets, or
arbitrary filesystem paths. Hosted plugins run out of process through the existing
jail. Treat renderer HTML as untrusted: sanitize it and serve active artifacts in a
sandboxed origin.

## AI is an action, not ambient authority

Initial AI actions should be deliberately small:

- explain a cell or error;
- propose a fix;
- generate a Python or Mojo cell;
- optimize selected Python into Mojo, gated by compile and parity checks;
- summarize or document the notebook.

An AI action receives the selected cell, explicit dependency context, the user's
instruction, and a bounded output/error excerpt. It returns `ProposedEdit` objects.
The UI shows a diff and the user applies or rejects it; only then is a revision
created and downstream cells marked stale. Generated code never executes
automatically.

Model traffic uses the existing `/v1/agent/*` proxy and credit settlement. Store
model, token usage, plugin version, input revision, proposed patch, and the user's
decision as an audit event. Never put provider keys in the browser or kernel.

## Product flow

1. Open or import a notebook; Marimo `.py` files convert without execution.
2. Save immutable source revisions while outputs remain separate content-addressed artifacts.
3. Build the reactive graph locally and request a placement quote before remote work.
4. Run Python in the browser where possible; use existing tiers for packages, Mojo, GPU, or long-running cells.
5. Stream per-cell state and settle measured usage through existing billing.
6. Apply plugin or AI edits as a reviewed revision, then rerun only descendants.

## First delivery slice

- Notebook list, create, import, open, save, and revision APIs.
- Markdown/Python/Mojo editor with run-cell and run-all.
- Marimo import and Markdown export.
- Python and Mojo execution through the existing planner and runner.
- `ai.explain`, `ai.fix`, and `ai.generate-cell`, all diff-before-apply.
- Allowlisted built-in plugins only; third-party installation follows capability enforcement and audit logs.

## Non-negotiable tests

- Plugin collision and denied-capability tests.
- AI can propose but cannot mutate or execute a cell.
- Revision conflict and idempotent action-apply tests.
- Tenant isolation for source, outputs, datasets, caches, and agent context.
- Planner and billing parity with ordinary mojojojo runs.
- HTML renderer sanitization and artifact-origin isolation.
- Marimo import fixtures followed by a clean reactive run.
