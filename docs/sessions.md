# Moving sessions between Claude Code and a profile

A profile runs Claude Code with its own config directory, so its sessions are separate from yours.
Two commands move chosen sessions across:

```sh
ccgp import zai        # the 10 latest sessions of your Claude Code -> the zai profile
ccgp export zai 3      # the 3 latest sessions of the zai profile -> your Claude Code
```

`count` defaults to 10 and is taken over **all projects**, newest first (by the transcript's modification time).
Afterwards run `claude` (or `ccgp <profile>`) in the same working directory and use `/resume`:
Claude Code lists sessions per project directory.

## What is copied

`projects/<project>/<id>.jsonl` and the `<id>/` folder next to it (tool results, subagent transcripts), with the
original modification time. Nothing else crosses: no credentials, no prompt history, no live-process registry,
no `file-history` (so `/rewind` is unavailable for copied sessions).

## Safe to repeat

Transcripts only grow, so ccgp compares as it copies, in a stream (gigabyte sessions need a few MB of memory):

| Result | Meaning |
| --- | --- |
| `copied` | The destination had no such session. |
| `updated` | The destination was an earlier part of the source; the missing tail was appended. |
| `unchanged` | Identical. |
| `ahead` | The destination already has more; left alone. |
| `diverged` | Both sides changed differently; the destination is left alone. |

Nothing is ever overwritten. A session still being written is copied up to its last complete line.

## Thinking blocks

Thinking-block signatures are valid only for the provider that produced them (Codex: `ccgp1:…`, z.ai: its
own format), and the official API rejects foreign ones. Therefore:

- **`export`** removes `thinking` and `redacted_thinking` blocks. Claude Code writes one line per content block,
  chained by `parentUuid`; a line left empty is dropped and its children are re-attached to its parent, so the
  conversation stays connected. Every other line is copied byte for byte.
- **`import`** keeps everything: Codex ignores signatures that are not its own, z.ai accepts them.

## Boundaries

`import` only reads your Claude Code directory. `export` is the one command that writes into it, under
`projects/` only, and only adds or extends files. The directory is `$CLAUDE_CONFIG_DIR` if set, else `~/.claude`.
