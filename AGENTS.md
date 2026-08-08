# Foundry - A CLI for managing individual pieces of a Catalyst Community tech stack and workflows within that stack

## Project Status

The system is actively being developed. Join the [Catalyst Community Discord](https://discord.gg/sfNb9xRjPn) to discuss and contribute.

## Best Practices

- We do not use .env files except for Docker because that is required, everything else is just a config file in yaml
- We write tests for every feature including the happy path and error paths
- We only mock third party APIs, everything we can run locally we'll spin up a container based dev/test environment
- We ensure all tests pass before we mark tasks complete
- We try to separate concerns. Things should not try to control too many other things.
- We use semantic versioning and conventional commits.

## Go Conventions

- **Accept interfaces, return structs.** A function that needs a dependency should
  take the narrowest interface that describes what it uses, not a concrete type.
  Constructors return concrete types so callers keep the full API.
- **Reuse the interface that already exists** rather than declaring a parallel one.
  For example, anything that reads or writes a secret takes
  `k3s.KubeconfigClient` (or a similarly narrow interface); `*openbao.Client`
  satisfies it structurally, so no adapter is needed.
- This is what makes the "only mock third party APIs" rule practical: OpenBAO,
  Helm, and the Kubernetes API are reachable through interfaces, so unit tests
  substitute a fake for the external service and exercise real logic against
  synthetic data. Taking a concrete client instead forces an integration test for
  logic that is otherwise pure.
- Prefer injecting the dependency as a parameter over constructing it inside the
  function. Where a command must build its own client, split the decision logic
  into a helper that receives the interface (see
  `reconcileKubeconfigEndpointWithClient` in
  `v1/cmd/foundry/commands/component/install.go`).

## Documentation Guidelines

- **Task tracking and checklists are fine** - We don't know ahead of time if work spans multiple sessions
- **Be cautious about creating user documentation** - When in doubt about writing guides, how-tos, or reference docs, **ask first**
- User-facing documentation goes in `docs/` when explicitly requested or clearly needed
- Design and architecture docs go in root (DESIGN.md, implementation-tasks.md, etc.)
- Don't create documentation "just in case" - wait until it's actually needed

