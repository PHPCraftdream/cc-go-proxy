# Codex Fast mode: proposed CLI

Status: design proposal. The commands below are not implemented.

Fast requests faster generation from the same Codex model. It can consume more allowance than Standard. It does not select a model, lower reasoning effort or change how a profile is launched.

## Command

```sh
ccgp --speed                          # List explicit speed rules
ccgp --speed codex astra              # Show the policy for this pair and its source
ccgp --speed codex astra fast         # Always request Fast
ccgp --speed codex astra standard     # Use Standard even if the client requests Fast
ccgp --speed codex astra inherit      # Remove the rule; follow the client again
```

`--speed <profile> <family>` follows the profile-first order of `--which`. Omitting the value reads the setting; it never changes it. A separate command keeps the existing `--profile set <name> <key> <value>` grammar intact. `ccgp codex …` remains a launcher: its arguments still go to Claude Code.

## Rule resolution

Store the policy by **profile and model family**, not by provider, Claude alias or versioned backend model ID. For example, `codex` and `codex2` can have different policies for `astra` even if they share a provider. Two Claude aliases that route to the same Astra in one profile use the same policy.

Resolve the model through normal routing first. `astra` means the current Astra available from that account's catalog; do not pin a release in the command, saved rule or matching logic. When routing moves to a new Astra release, the rule follows without another command. A speed rule never redirects a request for another family to Astra.

With no rule, honor an explicit client Fast request; otherwise leave the ordinary Codex request unchanged. `inherit` deletes the override rather than persisting a third mode. If the backend rejects an explicit Fast request, report the reason instead of silently retrying in Standard.

## Implementation checks

- Confirm the request signal Claude Code sends with `/fast` on versus off, and the service tier the official Codex client sends for the resolved model. Do not infer activation from a capability header alone.
- Apply the tier to the common Codex request before transport selection. Cover HTTP, WebSocket and a mode change between turns on a WebSocket delta chain. Model and reasoning effort must remain unchanged.
- Mirror the command in `--interactive`: profile → family → mode. Include it in help, completion and CLI/TUI parity tests when implemented.
