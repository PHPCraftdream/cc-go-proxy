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
original modification time, and what Claude Code keeps of the session outside its transcript:

| Artifact | Where | How it is copied |
| --- | --- | --- |
| the task list | `tasks/<id>/<n>.json`, `.highwatermark` | made the source's: a task that differs is written anew, one the source deleted is taken out; a file changed at the destination after the source's, and a task taken out, is kept as `<name>.bak-<time>` (Claude Code reads `*.json` only) |
| file versions for `/rewind` | `file-history/<id>/<hash>@v<n>` | like the transcript: a missing version is written, one the destination holds differently is left alone (`--force`: replaced, the old one kept) |
| the plan | `plans/<slug>.md`, the slug is in the transcript | written when it differs; one changed at the destination after the source's is kept as a backup first |

They follow the transcript: a session the destination went its own way on (`ahead`, `diverged`, `elsewhere`) keeps its
own tasks, versions and plan untouched, and a session copied before ccgp copied them gets them on the next run (the
line says `task list: 36 files, file versions for /rewind: 3655, plan: 1`). A fork has them as its own: its task list
and file versions under its id (task numbers as they are, file versions byte for byte), its plan under a slug of its
own (`<slug>-<first 8 of its id>`, which its transcript names), so that the fork and the original, in one Claude Code,
do not write the same plan.

Nothing else crosses: no credentials, no prompt history, no live-process registry, no `session-env/<id>/` (hooks write
it anew when a session starts or resumes, and a profile's holds ccgp's own environment).

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
| `elsewhere` | The destination has a session of this id in another project folder (another directory); nothing is written. `--fork` writes it under a new id. |

Without a flag nothing is ever overwritten. A session still being written is copied up to its last complete line.

## When a session diverged

A session can be continued on both sides: you went on in the profile, and also in your own Claude Code (or the same
session lives in two profiles). The two files then share a beginning and differ after it, and `ccgp --send` does not
guess which one is right: it reports `diverged`, leaves the destination alone, and `ccgp <profile> --resume <id>` opens
the destination's version, not the source's. Two flags say what to do (they work with `--send`,
`--import` and `--export`):

```sh
ccgp --send claude codex 1 --fork          # keep both
ccgp --send claude codex 1 --force         # take the source's version
ccgp --send claude codex 1 --fork --force  # keep both, and write the fork afresh
```

Together, `--fork --force` forks a diverged session as `--fork` does, and a fork the destination already holds that
went its own way (written by an earlier ccgp, or continued there) is renamed to `.bak-<time>` and written afresh. The
original is never touched, and a session that did not diverge is copied as without the flags.

- `--fork` writes the source as a **new session** in the destination, with a new id (the same id on every run, made
  from the original's, so the next run finds it and only appends what came after). Every id of the session is
  replaced, in every line, in the files of its folder and in their names: the session id, the session's uuids (every
  uuid that is a field's value somewhere in it: messages, prompts, …), the API's `toolu_…` / `msg_…` / `req_…` ids,
  the Responses API's `call_…` / `resp_…` / `fc_…` / `rs_…` ids of a session run on Codex, the ids of subagents
  (`agent-<id>.jsonl`, `.meta.json`) and of background tasks (`tool-results/<id>.txt`; a task id of letters only is
  taken when it has the form `b` + 8 letters; one named in text first, as in `with ID: b…` or `tasks/b….output`, is
  known from there, so a fork synced while the task runs is extended later, not diverged), wherever they stand, text
  included. A uuid only
  mentioned in text (a link to something else) is not the session's and stays, and so does a word an id field holds.
  Each old id always gets the same new one, so tool calls keep their results, subagents their files, and the
  next run extends the fork; a fork that kept any id of the original would be the same thing to Claude Code in places
  (with the original's message ids `/resume` listed only one of the two). Files that are not text (images) are copied
  as they are. The `session-owner` lines are left out: they name the account of the source's Claude Code, and Claude
  Code stamps a transcript without one with the account that opens it (a copy without `--fork` keeps them, as copies
  made before did, so it stays in step with them). If the fork's id is already a session in another folder of the
  destination, it is reported `elsewhere` and not written a second time. The old session stays
  as it was. The run says `open a fork with: ccgp <profile> --resume <new id>`. If the fork was continued on its own
  too, it diverges in turn: it is reported and left alone (move or delete that file to get a fresh fork).
- `--force` replaces the destination with the source. The old file is renamed to `<id>.jsonl.bak-<time>` in the same
  folder, so nothing is lost (a backup is not a session: `/resume` does not list it; rename it back to
  `<id>.jsonl` to return to it). It also applies to a destination that is `ahead`. Files of the session's folder
  (`<id>/`) that differ are replaced the same way, each with its own `.bak-<time>`.

**Sessions of a subdirectory.** The sessions of the directory you run `--send` in include those Claude Code moved into
the folder of a directory below it (it does so with a session continued in a worktree): a folder whose name starts
with the directory's and whose transcript starts in the directory itself (a session begun here and continued in a
worktree records the directory first), judged session by session: a worktree's folder also holds the sessions begun in
the worktree, and those, like the sessions begun in any subdirectory, stay that subdirectory's. Such a session is
written into the
folder of the directory you run it in, and its `relocated` lines are made to name that directory: Claude Code lists a
session under the last `relocatedCwd` of its transcript (before the `cwd` of its first lines; a marker line is known by its type,
whatever the order of its fields), so with the worktree the
source names, `/resume` in the directory would hide it (Ctrl+W there shows the worktrees' sessions). The line says
`(of a subdirectory, written into <folder>)`.

**Sessions begun in another directory.** A session in the directory's folder whose transcript starts somewhere else
(Claude Code's `/cd` moved it here, or an earlier copy put it here) is listed by Claude Code under the `cwd` of its first
lines unless the final 64 KiB of its transcript hold a `relocated` line; on Windows, in a repository with worktrees,
`/resume` in the directory then shows none of it. The copy names the directory in its `relocated` lines and adds one
after each `last-prompt` line (Claude Code writes that line with a session's metadata at the end of every turn). The
place is fixed by the line, not by the end of the file, so a copy of a session that grows is still extended in place.
A copy written before this change differs from the new one: `--send` reports it `diverged`, and `--force` (or a fork
removed first) writes it afresh.

The same id can also sit in **another project folder** of the destination: Claude Code moves a session into the folder
of a worktree it enters, on one side only. Its id is then taken by a different session there, and a copy with that id
would make two sessions of one id. ccgp reports `elsewhere` (with the folder) and writes nothing, `--force` included;
`--fork` writes it under a new id in the folder of the directory you run it in. Into your own Claude Code the run says
`open it with: claude --resume <new id>`.

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

## A failed end is left out

A session can end in turns that got no reply: Claude Code writes `API Error: …` in place of the answer (a usage
limit, a server error), often once per tick of a scheduled prompt (`/loop`, a babysit cron) that kept firing while
the limit lasted. A copy (plain, `--fork` or `--force`) leaves that end out and goes on from the last real exchange,
so the copy resumes there. The report says `left out its failed end: N API errors with no reply after them`.

The end left out is what follows the last line of the conversation (a prompt you typed, a reply, a tool result,
an attachment, a task notification) and the session state written right after it (title, mode, last prompt). It
holds only API error messages, the records of the requests that failed, the turn durations and scheduled prompts
around them, and the `[Request interrupted by user]` notes of turns you stopped with Esc. Nothing is left out when that part holds no API error, when it is longer than 64 MiB, or when the
transcript has no other line. Only the session's own transcript is looked at, not its subagents'.

The rule stays append-only: once the session goes on, the errors are no longer its end and come along with the
next copy. A copy made earlier that already holds the failed end, or part of it, is `unchanged`, not `ahead`.

## Boundaries

`claude` as the **source** only reads your Claude Code directory. `claude` as the **destination** is the only
way ccgp writes into it, under `projects/` only, and only adds or extends files (`--force` into `claude` renames the
file it replaces, and keeps it). The directory is
`$CLAUDE_CONFIG_DIR` if set, else `~/.claude`.
