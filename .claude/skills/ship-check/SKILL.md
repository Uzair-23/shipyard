---
name: ship-check
description: Run before committing a finished task.
---
1. Run `make lint` and `make test`; fix failures.
2. If files under runtime/, proxy/, builder/ or auth/ changed, use the security-reviewer agent.
3. Append what was done and any open issues to docs/PROGRESS.md.
4. Propose a conventional commit message. Do not push.