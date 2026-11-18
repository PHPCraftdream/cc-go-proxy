# Codex Fast mode

Fast requests a faster service tier for the same Codex model. It does not change the model, reasoning effort or profile launch. Fast may consume more allowance; account entitlement, quota and availability are controlled by the backend.

```sh
ccgp --speed                         # List explicit rules
ccgp --speed codex astra             # Show the rule or inherited policy
ccgp --speed codex astra fast        # Request Fast for this profile and family
ccgp --speed codex astra standard    # Suppress a client's Fast request
ccgp --speed codex astra unset       # Remove the rule
ccgp --speed codex astra inherit     # Same as unset
```

The interactive interface also edits these rules under Config → Speed. Rules are saved in `config.ktav` per Codex profile and model family, not per provider or client alias. The family is determined from the resolved backend model, so a newer Astra model follows the same `astra` rule without changing it. Other profiles and families are unaffected. The command edits the config with validation and a backup; reads do not change it.

With no rule, an explicit Messages request `speed: "fast"` requests Fast; standard, absent or null speed leaves the tier unset. A `fast` rule requests Fast regardless of the client selection; a `standard` rule leaves the tier unset even when the client requests Fast. Client-side `/fast` availability checks and model restrictions can prevent the client from selecting Fast; a capability header alone is not a selection.

For an effective Fast request, ccgp looks up the resolved model in the account's Codex catalog and sends the ID of its `Fast` service tier. Successful lookups are cached briefly. A missing tier or catalog/authentication failure returns an error rather than silently retrying as Standard. Ordinary requests do not require a Fast catalog lookup. The tier is applied to HTTP and WebSocket requests.

**Requested is not actual:** setting a rule or sending a tier does not establish which speed the backend actually used, guarantee lower latency or grant entitlement. ccgp does not manufacture an actual-speed confirmation. Backend limits, quota, billing and behavior can differ from the client's; consult the provider for current terms. See [protocol evidence](codex-fast-protocol.md) for the translation's basis and limits.
