---
name: router-models
description: "List the models the Weave Router may route to, and turn them on or off."
---

<!-- weave-router managed router-models skill -->

# Weave Router model selection

When the user invokes `$router-models`, show which models this
installation lets the router pick from, and change that selection when asked.
This is the same list — and the same stored setting — as the checkboxes on the
router dashboard's settings page.

Start by running:

```bash
{{ROUTER_CLI}} models --codex{{SCOPE}}
```

That prints every deployed model grouped by provider, with `[x]` for models the
router may pick and `[ ]` for models it may not, and each model's class —
`(high)`, `(mid)` or `(low)`, the value the `x-weave-model-class` request
header selects on — after its id when it has one. Keep the class next to each
id in what you show, e.g. `claude-haiku-4-5 (low)`. Present it back grouped by
provider, with an **On:** line and an **Off:** line under each provider listing
the exact model ids (comma-separated). Don't write `[x]` / `[ ]` or a markdown
task list — chat renders checked tasks struck through, which reads as disabled.
Keep the exact model ids — the user selects models by id.

Then:

- If the user named models or providers, work out whether they want them on or
  off from how they phrased it, and apply it with
  `{{ROUTER_CLI}} models enable <id>... --codex{{SCOPE}}` or
  `{{ROUTER_CLI}} models disable <id>... --codex{{SCOPE}}` (add `providers` before
  `enable`/`disable` to switch a whole provider). Several ids can go in one
  call. Then re-run the list and show the result.
- If they named nothing, stop after the list and ask which ones to change.
  Never change anything they did not ask for.

Other things they might ask for:

- Rank models by preference:
  `{{ROUTER_CLI}} models prefer <id> <id>... --codex{{SCOPE}}` (order matters), or
  `{{ROUTER_CLI}} models prefer clear --codex{{SCOPE}}` to drop the ranking.
- Providers only: `{{ROUTER_CLI}} models providers --codex{{SCOPE}}`.

If the command reports that this router doesn't expose model selection, that's a
Weave-hosted router: model selection belongs to the whole organization there, so
tell the user to change it at https://router.workweave.ai/dashboard/settings —
don't try to work around it. Its listing carries no on/off state, so present it
as a plain list; don't infer which models are enabled.

If an `enable`, `disable` or `prefer` is refused with a message naming the
router dashboard, this router keeps model-selection changes in its dashboard:
show the user that message and its link as they are, and don't retry or work
around it. Listing keeps working.

Disabling a model takes effect on the router's next routing decision; no restart
is needed.
