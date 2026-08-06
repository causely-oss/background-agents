# background-agents

Examples of giving background agents context to reason about a live system and resolve issues quicker, with using tokens and no helucinations. 

A background agent with read access to metrics, logs, and traces and predefined run-books still can't tell cause from symptom. The example show how to add the casual context the agent needs to be affective.  

Each example is self-contained and runnable.

## Examples

| Example | What it shows |
|---------|---------------|
| [`claude-managed-agents/`](./claude-managed-agents) | A [Claude Managed Agent](https://platform.claude.com/docs/en/managed-agents/overview) investigating a live Kubernetes cluster through remote MCP servers: reaching a local cluster from a cloud agent, three auth patterns (none, static bearer, OAuth client-credentials), and evolving the system prompt from mock data to real tools. Runnable against a local [kind](https://kind.sigs.k8s.io/) cluster. |
| [`open-inspect/`](./open-inspect) | [Open-Inspect](https://github.com/ColeMurray/background-agents)'s OpenCode runtime wired to a remote MCP server (Causely) so its background investigations cite root-cause and log evidence in the PRs they open, instead of guessing from source. Custom-header auth. |

## Reach and auth, in brief

- **Reach.** The agent runs on a different network than the cluster it's investigating and can't see `localhost` or a private API server. Something bridges that gap without exposing your systems publicly.
- **Auth.** Non-interactive, no browser. What the agent can send depends on the
  client: a custom header where the client supports one (Open-Inspect), or a
  minted `Bearer` token where it doesn't (Claude Managed Agents). Same MCP
  server, different auth.

## Getting started

Pick an example and follow its README:

- **[`claude-managed-agents/`](./claude-managed-agents)** — a runnable
  end-to-end setup against a local cluster.
- **[`open-inspect/`](./open-inspect)** — if you already run Open-Inspect and
  want its investigations to cite live root-cause evidence in the PRs they open.

## Contributing

New examples welcome, especially ones that give an agent a usable model of the
system a different way, or cover a different framework or class of system. Open
an issue before a large PR.

## License

[Apache-2.0](./LICENSE).
