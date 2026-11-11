---
sources:
    - path: internal/featuredoctor/feature.go
      sha256: 4bfa28ea66b60d6d64c9f551654d49723b440d9fa60eca45b63e771346d40525
---
# The feature doctor

Several of vornik's capabilities — [memory & RAG](../features/memory-rag.md),
[authentication](../features/auth.md), the [instinct layer](../features/instinct.md),
[clustering](../features/cluster.md) — are opt-in and gated behind config flags.
The **feature doctor** is the single tool for diagnosing and turning them on
safely, so you don't have to hand-edit config and guess whether you got the
prerequisites right.

## Diagnosing

To see the state of a feature — whether its gates are set, whether its
prerequisites are met, and what's left to do:

```bash
vornikctl doctor feature <feature-id>
```

Each feature reports its gates (the config keys that enable it) and runs
**prerequisite checks** — for example, memory & RAG probes that an embedding
model is actually reachable, and auth checks that an admin key is in place
before you lock the door behind you. The doctor tells you what's missing and how
to fix it rather than failing cryptically at runtime.

## Enabling

To apply a feature's gates and bring it up:

```bash
vornikctl doctor feature enable <feature-id> --apply
```

Without `--apply` the command is a **dry run**: it prints the proposed gate
changes and how they would be applied, and mutates no config. With `--apply`
it backs up `config.yaml`, writes the gates, then either reloads the daemon
and verifies, or (for restart-gated features) validates the written file and
tells you to restart. If any step fails, it rolls back to the backup.
Two behaviours make this safe:

- **Prerequisite-gated.** Enable refuses to proceed if a prerequisite isn't met
  (for example, the embedding endpoint is unreachable), so you don't half-enable
  a feature into a broken state.
- **Restart-aware.** Some features take effect only on a daemon restart. For
  those, enable **refuses** while tasks are active rather than waiting — it
  tells you to wait for an idle window and try again. On an idle daemon it
  writes the config and reports `RestartRequired`: the doctor never restarts
  the daemon itself, so you restart vornik when it suits you. Enabling a
  feature therefore never interrupts work in flight.

## Why use it instead of editing config

The config keys are documented, and you *can* set them by hand. The doctor adds
the parts that are easy to get wrong: it validates the prerequisites first,
applies the full set of gates a feature needs (not just the obvious one), and
handles the restart timing. Each feature page in this documentation names the
doctor command that enables it; that's the recommended path.
