# Choose the models your roles run on

Every role in the swarms Vornik ships names a **model tier**, not a model.
Your instance decides which model serves each tier. Out of the box, every
tier runs on the model you chose during setup, so a one-model install needs
nothing from this page.

Map tiers when you have more than one model and want each role on a model
that suits its job: a strong model for planning, a cheap one for yes/no gates.

## The five tiers

| tier | used by | wants |
|---|---|---|
| `complex` | leads, analysts, planners, strategists, reviewers of designs | the strongest reasoning model you have |
| `coding` | coders, test writers, code reviewers | a strong agentic coder that drives tool calls reliably |
| `standard` | mechanical steps: publishing, order placement, file copying | a fast, cheap model that follows instructions |
| `trivial` | gates and classifiers that return a short JSON verdict | the cheapest model that returns valid JSON |
| `vision` | roles that look at images | a model that accepts images and calls tools |

A tier you do not map runs on the setup model.

## Map tiers yourself

Add the maps to `runtime.agent_llm` in `config.yaml`, then restart the daemon:

```yaml
runtime:
  agent_llm:
    model: "my-default-model"          # the setup model
    tiers:
      complex: {model: "big-model", fallback: "other-big-model"}
      trivial: {model: "small-model"}
    roles:                              # optional: one role, ahead of its tier
      dev-swarm/coder: {model: "big-model"}
```

A role resolves its model in this order:
1. the `model` set on the role itself, in its swarm file;
2. its `"<swarm>/<role>"` entry in `roles`;
3. its tier's entry in `tiers`;
4. the setup model.

The `fallback` is tried once when a step fails on the first model in a way
that looks like the model's fault (a schema violation, an iteration limit).
Choose a fallback from a different provider, so one outage does not take
out both.

## Or apply a provider profile

Vornik ships profiles for two providers whose model IDs it has run:

```bash
vornikctl models profile list              # marks the profile that matches your endpoint
vornikctl models profile apply bedrock     # dry run: shows what it would write
vornikctl models profile apply bedrock --apply
```

| profile | complex | coding | standard | trivial | vision |
|---|---|---|---|---|---|
| `bedrock` | `zai.glm-5` | `moonshotai.kimi-k2.5` | `minimax.minimax-m2.5` | `zai.glm-4.7` | setup model |
| `ollama-cloud` | `glm-5.2` | `kimi-k2.6` | setup model | setup model | `gemma4:31b` |

On 2026-11-22 each ID in these profiles answered a live request that offered
one tool, by calling that tool. A tier a profile leaves out runs on the
setup model: no model for it passed that check.

Applying a profile replaces `runtime.agent_llm.tiers` and `.roles` and
changes nothing else. After that the maps are your configuration: edit them
freely, and an upgrade never rewrites them. Providers retire models, so a
profile applied months ago can name a model that no longer answers;
`vornikctl doctor` reports failing models.

For OpenRouter, an on-prem server or any other provider, there is no
profile: map the tiers yourself with the IDs your provider serves.

## Saving a model checks it first

When you save a role's model or fallback, apply a profile, or create a
project from a template with a model, Vornik first makes one short call to
that model through your providers, offering it one tool:

- **It does not answer:** the save is refused with the provider's reason,
  and nothing is written.
- **It answers without calling the tool:** the save goes through with a
  warning. Agent roles need tool calls, so check the model before you rely
  on it.
- **It calls the tool:** the save goes through. The result is remembered
  for 10 minutes.

Only models the save changes are checked, and each check takes at most 15
seconds. If a provider is briefly down, tick **Save without checking
models** in the console's swarm editor, or add `--force` to
`vornikctl models profile apply`; both are recorded in the admin
audit. On a managed instance a model outside your catalogue is refused even
then.

## Check what each role runs on

```bash
vornikctl models tiers     # each role: its tier, the model it resolves to, and why
vornikctl doctor           # model_tiers: warns on a mapping that cannot take effect
```

The doctor warns about a key that is not a tier (a typo such as `coplex`), a
tier no role uses, or a `roles` key that matches no role. A role running on
the setup model is never a warning.

## On a managed instance

Your hosting service sets the tiers, through `setup/managed`, from the models
in your catalogue. A tier can only name a catalogue model.
