# AGENTS.md

Working agreements for this repo. They apply to agents and humans alike.

## Scope

- Do the basic thing first.
- **Do not add features without discussing them.** Something that was not asked for is not a bonus.
- Less but functional beats more but broken.
- Keep changes scoped to the task at hand. No unrelated refactors.

## Context

- Read the minimal local context the task requires.
- Delegate exploratory or noisy work to a subagent — broad code search, multi-file investigation,
  log and test-output trawls. The subagent returns the conclusion; its intermediate tool output
  stays out of the main context.

## Comments

- Code comments document **behaviour, for a future reader**.
- They do not document the reasoning behind the current change. That belongs in the commit message.

## Documentation

- Detailed product documentation belongs in `docs/` and is published with Mintlify.
- Keep `README.md` focused on the product, the working demo, and the shortest paths to evaluation.
