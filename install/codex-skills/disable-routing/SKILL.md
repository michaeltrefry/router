---
name: disable-routing
description: "Switch Codex back to its normal provider for future sessions."
---

<!-- weave-router managed disable-routing skill -->

# Disable Weave routing

When the user invokes `$disable-routing`, switch the current Codex installation
off the Weave Router without logging out or deleting its router configuration.

1. Explain that the change takes effect on the next `codex` launch and that it
   can later be reversed with `{{ROUTER_CLI}} on --codex{{SCOPE}}`.
2. Run exactly:

   ```bash
   {{ROUTER_CLI}} off --codex{{SCOPE}}
   ```

3. Report the command result. Do not uninstall the router or alter any other
   Codex settings.
