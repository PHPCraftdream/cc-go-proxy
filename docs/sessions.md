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
ccgp --import zai 1 --fork   # a session that was continued on both sides: keep both versions
ccgp --import zai 1 --force  # ... or replace the profile's version (the old file is kept)
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

## The first launch of a profile

When `ccgp <profile>` starts and the profile has no session of the **current directory** (a new profile, or one that
has only worked in other directories so far), ccgp copies the one latest session of this directory from your Claude
Code first, the same way as `ccgp --import <profile> 1`, so `/resume` or `-c` can carry on with the work at once
(Claude Code lists the sessions of the current directory only: without one, `-c` says `No conversation found to
continue`). It happens once per directory: after that the profile has a session of it and nothing is copied at
launch. If Claude Code has no session for this directory, nothing is copied and nothing is printed. A copy that
fails only prints a note on stderr; the profile starts anyway. Profiles of type `claude` (the real Claude Code,
[configuration](configuration.md#profiles-of-the-real-claude-code---type-claude)) are created clean and get no such
copy; bring a session in on purpose with `ccgp --import <profile>`.

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
| `forked` | `--fork`: it diverged, so the source was written as a new session next to the old one. |
| `replaced` | `--force`: the destination was replaced; the old file is kept beside it. |

Without a flag nothing is ever overwritten. A session still being written is copied up to its last complete line.

## When a session diverged

A session can be continued on both sides: you went on in the profile, and also in your own Claude Code (or the same
session lives in two profiles). The two files then share a beginning and differ after it, and `ccgp --send` does not
guess which one is right: it reports `diverged`, leaves the destination alone, and `ccgp <profile> --resume <id>` opens
the destination's version, not the source's. Two flags say what to do (they exclude each other and work with `--send`,
`--import` and `--export`):

```sh
ccgp --send claude codex 1 --fork    # keep both
ccgp --send claude codex 1 --force   # take the source's version
```

- `--fork` writes the source as a **new session** in the destination, with a new id (the same id on every run, made
  from the original's, so the next run finds it and only appends what came after). The id is swapped in every line of
  the transcript and in the transcripts of its subagents; tool results are copied as they are. The old session stays
  as it was. The run says `open a fork with: ccgp <profile> --resume <new id>`. If the fork was continued on its own
  too, it diverges in turn: it is reported and left alone (move or delete that file to get a fresh fork).
- `--force` replaces the destination with the source. The old file is renamed to `<id>.jsonl.bak-<time>` in the same
  folder, so nothing is lost (a backup is not a session: `/resume` does not list it; rename it back to
  `<id>.jsonl` to return to it). It also applies to a destination that is `ahead`. Files of the session's folder
  (`<id>/`) that differ are replaced the same way, each with its own `.bak-<time>`.

Sessions that do not diverge are copied as always. The interface asks the same in the *Sessions* screens (copy / copy and
fork / copy and force).

## Thinking blocks

Thinking-block signatures are valid only for the provider/account that produced them (a Codex `ccgp1:…`
signature carries encrypted reasoning another account may not decrypt), and the official API rejects foreign
ones. Therefore thinking and `redacted_thinking` blocks are removed whenever the source and destination
**providers** differ — `claude` counts as a provider of its own — and kept only between two profiles on the
same provider. This applies to `--import` too.

Claude Code writes one line per content block, chained by `parentUuid`; a line left empty is dropped and its
children are re-attached to its parent, so the conversation stays connected. Every other line is copied byte
for byte.

## Boundaries

`claude` as the **source** only reads your Claude Code directory. `claude` as the **destination** is the only
way ccgp writes into it, under `projects/` only, and only adds or extends files (`--force` into `claude` renames the
file it replaces, and keeps it). The directory is
`$CLAUDE_CONFIG_DIR` if set, else `~/.claude`.
