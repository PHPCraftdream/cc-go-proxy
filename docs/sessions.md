# Moving sessions between profiles and Claude Code

A profile runs Claude Code with its own config directory, so its sessions are separate from yours and from
other profiles'. `ccgp --send` moves the sessions of **the directory you run it in** across; `--import` and `--export`
are aliases:

```sh
cd /path/to/myproject
ccgp --send codex codex2 3   # the 3 latest sessions of this directory in profile codex -> profile codex2
ccgp --send zai claude       # the 10 latest sessions of this directory in the zai profile -> your Claude Code
ccgp --send claude zai       # this directory's latest sessions in your Claude Code -> the zai profile
ccgp --import zai            # alias of: ccgp --send claude zai
ccgp --export zai 3          # alias of: ccgp --send zai claude
```

`from` and `to` are a configured profile name or the word `claude` (your own Claude Code directory); they must
differ. Only sessions of the current working directory are copied (Claude Code keeps them in
`projects/<directory name>/`, where every character outside `A-Za-z0-9` of the path becomes `-`, so
`D:devmyproject` is `D--dev-myproject`); other directories and subdirectories are not touched. `count` defaults to 10,
must be a positive integer and counts that directory's sessions, newest first (by the transcript's modification
time). Afterwards run `ccgp <profile> -c` (continue the latest) or `ccgp <profile>` and `/resume` in the same
directory: Claude Code lists sessions per project directory.

Every session is announced as `[k/N] project/id  size ...`; one that takes longer than a couple of seconds also
prints a percentage line every two seconds, so gigabyte transcripts show that they are moving.

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

Thinking-block signatures are valid only for the provider/account that produced them (a Codex `ccgp1:…`
signature carries encrypted reasoning another account may not decrypt), and the official API rejects foreign
ones. Therefore thinking and `redacted_thinking` blocks are removed whenever the source and destination
**providers** differ — `claude` counts as a provider of its own — and kept only between two profiles on the
same provider. This applies to `import` too, which used to keep the blocks.

Claude Code writes one line per content block, chained by `parentUuid`; a line left empty is dropped and its
children are re-attached to its parent, so the conversation stays connected. Every other line is copied byte
for byte.

## Boundaries

`claude` as the **source** only reads your Claude Code directory. `claude` as the **destination** is the only
way ccgp writes into it, under `projects/` only, and only adds or extends files. The directory is
`$CLAUDE_CONFIG_DIR` if set, else `~/.claude`.
