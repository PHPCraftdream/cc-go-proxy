# Peak hours

Some providers charge more, throttle, or are simply slow at set hours of the day (z.ai's plan counts usage higher in
its afternoon, for instance). ccgp can know those hours and **refuse the requests of a provider's profiles while they
last**, so a long session does not eat the quota at the expensive time. You decide when to work; Claude Code tells you
why it stopped.

```sh
ccgp --peak zai set mon-fri 14:00-18:00      # the peak: Monday to Friday, 14:00 to 18:00 ...
ccgp --peak zai tz Asia/Shanghai             # ... in the provider's own time zone
ccgp --peak                                  # look at it
ccgp --peak zai pause 2h                     # work anyway for two hours
```

Peak hours belong to a **provider** (the service and its quota), so every profile on it is covered. Nothing is
configured by default.

## What happens in the peak

A request to `POST /call/<token>/v1/messages` that arrives inside a window is answered by ccgp at once, with an HTTP error:

```
400 invalid_request_error
Peak hours of provider zai: mon-fri 14:00-18:00 (Asia/Shanghai time). Requests resume at 18:00 Asia/Shanghai
(13:00 your time, in 2h 18m). To work anyway: ccgp --peak zai pause
```

Claude Code shows it in the chat as `API Error: 400 …` and, as with any 400 that is not a context overflow, does not
retry it (it retries 429 and 5xx up to ten times, which would look like a hang, so those statuses are not used). The message names the window, when requests resume in the
provider's zone and in yours, and the command that lets you through.

- **The provider never sees the request.** Nothing is sent, nothing is spent, no connection is opened, and the
  provider's prompt cache is neither used nor spoiled.
- **The refusal is not a reply of the model.** Claude Code keeps it as an error notice of its own. If it does send it
  back with the next request, it sits at the end of the conversation, after everything the provider has cached, so the
  cached prefix still hits.
- **Running requests are not cut.** A request that started before the peak finishes, a long stream too.
- **Everything else goes on:** token counting (`/v1/messages/count_tokens`), the model route, `--status`, `--login`.
- A refused request is one line in the log: `… 400 - POST /call/***/v1/messages [zai]: Peak hours of provider zai: …`.
- The server prints the peak hours of its providers when it starts (the first lines of the log, `ccgp --logs`).

## Windows

A window is `[days] HH:MM-HH:MM`:

| Part | Meaning |
| --- | --- |
| `days` | `mon tue wed thu fri sat sun`: one day (`sat`), a list (`sat,sun`), a range (`mon-fri`; `fri-mon` wraps the week), or a mix (`mon-wed,fri`). Absent: every day |
| `HH:MM-HH:MM` | From and to, 24 hours. **To is exclusive**: `14:00-18:00` ends at 18:00:00 and 18:00 is open. `24:00` is the end of the day |

- A window whose end is not after its start **runs past midnight**: `22:00-06:00` is the evening and the night after
  it. Its days are the days it **starts** on: `fri 22:00-06:00` covers the small hours of Saturday, not those of Friday.
- A whole day is `00:00-24:00`. A window of no length (`14:00-14:00`) is refused.
- Windows may overlap or touch (`14:00-18:00` and `18:00-20:00` are one peak until 20:00). A provider can have several.

The zone is the one the **provider** announces its hours in: a name (`Asia/Shanghai`, `America/New_York`: daylight
saving follows), an offset (`+08:00`, `-05:30`, `UTC+8`), `UTC`, or `local` (the machine's own, which is also the
default). The names work on Windows too: the zone database is built into ccgp.

## The command

`ccgp --peak [provider [verb …]] [--json]`; a profile name stands for its provider.

| | |
| --- | --- |
| `ccgp --peak` | Every provider: its windows, zone, whether it is peak **now**, and when that changes |
| `ccgp --peak <provider>` | The same for one |
| `set WINDOW…` | Replace all windows: `ccgp --peak zai set mon-fri 14:00-18:00 sat,sun 10:00-12:00` (no quotes needed) |
| `add WINDOW…` | Add windows (one that is there already is skipped) |
| `rm N…` | Remove windows by the numbers the list shows; removing the last drops the zone too |
| `tz ZONE` | The zone (windows first); `local` for the machine's |
| `clear` | Remove the peak hours (windows and zone) |
| `pause [LEN]` | Let requests through for `LEN` (`90m`, `2h`, `1d`, at most `7d`); without it until the running peak ends |
| `resume` | End the pause at once |
| `--json` | The state as JSON: `windows`, `zone`, `peak_now`, `until` (when it changes, RFC 3339), `paused_until` |

```
$ ccgp --peak
codex: no peak hours
zai: peak hours in Asia/Shanghai (there it is Tue 15:42 now)
  1  mon-fri 14:00-18:00
  2  sat,sun 10:00-12:00
  now: PEAK, requests are refused until 18:00 Asia/Shanghai (13:00 your time, in 2h 18m)
```

`set`, `add`, `rm`, `tz` and `clear` edit `config.ktav` the way the profile commands do (comments and line endings
stay, the result is validated with the real loader, a `config.ktav.bak-<time>` backup is kept) and the running server
applies the change on its next request. `pause` and `resume` do **not** touch the config: a pause is a state, kept
in `creds/peak-pause.ktav` (per provider, with an end time), and it ends by itself.

`ccgp --interactive` has the same as menus: *Peak hours → a provider* (or *Providers → a provider → peak hours*): add,
replace and remove windows, the zone, pause, resume, clear, with the state shown above the menu
([interactive mode](cli.md#interactive-mode)).

## In the config

```
{
    name: zai
    type: anthropic_compat
    base_url: https://api.z.ai/api/anthropic
    peak_tz: Asia/Shanghai
    peak: [
        {
            days: mon-fri
            from: 14:00
            to: 18:00
        }
        {
            from: 20:00
            to: 23:00
        }
    ]
}
```

`peak_tz` without `peak` is an error, and so is a bad window or zone: the loader names the provider, and a running
server keeps the previous settings until the file is right. A provider of type `claude` (the real Claude Code) has no
peak hours: its requests never pass through ccgp.
`ccgp --profile add NAME --like P` gives the new provider the peak hours of `P`.

## Questions

**Does it stop me from using the provider some other way?** No: ccgp only refuses the requests that pass through it.

**Why a refusal and not a delay?** A delayed request would hold Claude Code in a spinner for hours. An error says
what is going on and ends the turn; when the peak is over, say `continue`.

**Can I work in a peak once, without editing anything?** `ccgp --peak zai pause` (till the end of this peak) or
`pause 30m`. `ccgp --peak zai resume` brings the refusal back.

**The hours changed.** `ccgp --peak zai set …` (or edit `config.ktav`). There is no list of providers' hours inside
ccgp: they are yours to set.

**The machine's clock is wrong.** The windows are compared with the machine's clock converted to the zone, so a wrong
clock or zone moves the peak with it.
