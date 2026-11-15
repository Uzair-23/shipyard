#!/usr/bin/env bash
# PreToolUse hook for Bash. Exit 2 blocks the command and shows stderr to Claude.
# Enforces the "Never" list in CLAUDE.md (git workflow) and keeps private files out of commits.
input=$(cat)

block() {
  echo "guard-git: blocked - $1" >&2
  exit 2
}

# Destructive or history-rewriting git commands.
if grep -Eq 'git[[:space:]]+push[^"]*([[:space:]]-f([[:space:]]|")|--force)' <<<"$input"; then
  block "force-push is not allowed (CLAUDE.md: Never)."
fi
if grep -Eq 'git[[:space:]]+reset[[:space:]]+[^"]*--hard' <<<"$input"; then
  block "git reset --hard is not allowed (CLAUDE.md: Never)."
fi

# On commit, make sure nothing private is staged.
if grep -Eq 'git[[:space:]]+commit' <<<"$input"; then
  staged=$(git diff --cached --name-only 2>/dev/null)
  bad=$(grep -E '(^docs/|(^|/)\.env($|\.)|CLAUDE\.local\.md|\.pem$|\.key$|\.crt$|\.p12$|(^|/)master\.key$|(^|/)secrets/)' <<<"$staged" | grep -v '\.env\.example$')
  if [ -n "$bad" ]; then
    block "private files are staged for commit: $(echo "$bad" | tr '\n' ' ')- run 'git restore --staged <path>'."
  fi
fi

exit 0
