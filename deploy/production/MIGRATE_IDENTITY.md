# Gangolf DiVoid identity cutover

`migrate_identity.py` rehomes the existing private API records from principal3/root1 into a separate principal4-owned project. Record IDs, payload bytes, semantic edges, staging incompleteness and delivery states are retained. Processor root1, original containers1–8, archives, credentials and service settings remain intact. Only API/core are paused. Twitch messages during the maintenance interval may be missed; bootstrap does not provide lossless catch-up or durable exactly-once processing.

Preparation and fake tests do **not** authorize execution. Use the merged, reviewed helper only after Toni invokes the privileged step and Operations confirms the exact production containers, local endpoints and quiet window. Keep the current service running while waiting. Never paste an admin key into chat, a command, environment variable or file.

The fixed deployment is `tcp://gangolf:2375`, graph `http://gangolf:8091/api`, API `http://gangolf:8092/api`, canonical configuration `~/.codex/secrets/gangolf-production-canonical`, principal4 credential `~/.codex/secrets/.gangolf-divoid-gangolf`. Each credential/environment file must be an owned, single-link regular file with mode0600. The journal directory must be owned and private. No new package dependencies are required; use Python3 and the installed Docker CLI.

After merge, Operations installs the reviewed helper beside the existing production tooling in `~/.local/share/gangolf-production`. From Toni's interactive terminal:

```sh
python3 ~/.local/share/gangolf-production/migrate_identity.py check --journal ~/.codex/secrets/gangolf-production-canonical/identity-migration.json
python3 ~/.local/share/gangolf-production/migrate_identity.py apply --journal ~/.codex/secrets/gangolf-production-canonical/identity-migration.json
```

Both commands obtain the existing administrator key through hidden terminal input and verify `/users/me` permissions. `check` makes no graph/config/runtime writes. `apply` stays online through preflight and the final `QUIESCENT` confirmation. Operations must establish no active turn/send/receive writer. The helper additionally checks balanced Processor run events since core startup and no established Processor8080 connection twice, rechecks immediately before stopping core, then checks for a race after core stops. These conservative samples are not an atomic drain endpoint. Active or ambiguous activity requires investigation, not forced continuation.

After both writers stop cleanly, the helper takes a fresh full snapshot and refuses drift, unresolved graph references, unknown app types, unknown outbound delivery outcomes and staged outgoing shells. It creates five private containers as principal4, journals each operation before sending it, patches only owner/root, replaces existing canonical memberships and verifies original metadata/content/edges and Processor preservation. It changes exactly API token/root/messages bindings; CONFIG44 and every other environment value are retained. It preserves the stopped original API under a unique rollback name and creates its replacement from the original pinned image, Config and HostConfig. It validates replacement settings, API readiness, full authenticated v3 pagination and principal separation before restarting the unchanged core once. No message POST, replay, intent resend, model change, Processor restart or DiVoid restart occurs.

The private journal contains graph snapshots and mutation intent; a separate0600 `.api.env` backup preserves the prior API environment. Neither stores the entered administrator key. Do not print/upload either file, raw Docker inspection/logs, graph payloads or environment files. The preserved original API retains its original environment internally. Retain the journal, backup and stopped original API until Operations has completed acceptance and root authorizes housekeeping.

On a pre-binding failure, the helper attempts exact graph rollback and resumes the prior binding only after preservation verifies. If rollback cannot verify, writers stay stopped for recovery. To recover a known, pre-traffic partial migration:

```sh
python3 ~/.local/share/gangolf-production/migrate_identity.py recover --journal ~/.codex/secrets/gangolf-production-canonical/identity-migration.json
```

Recovery requires the administrator again, stops the journaled writers, restores exact prior owner/root/membership, verifies both edge projections/content/privacy, restores the protected API environment, then starts the prior API and core once. New containers remain private and unused. Unknown node/API creation responses are never blindly retried. Operations must reconcile an unknown created container's actual identity and exact settings against the private pending intent before recovery; preserve all candidates stopped. A journal marked `api-starting`, `resuming`, `resumed` or `traffic-reconciliation-required` refuses automatic inverse recovery because API traffic may already have changed the graph. Operations must freeze writers and reconcile new data with the owning session before authorizing any inverse changes. Do not hand-edit payloads, delivery states or owners to bypass these gates.

After a successful migration, repeating `apply` with the same journal refuses writes. Use:

```sh
python3 ~/.local/share/gangolf-production/migrate_identity.py verify --journal ~/.codex/secrets/gangolf-production-canonical/identity-migration.json
```

This verifies the recorded graph immediately after migration; later legitimate traffic can change the inventory, so a later mismatch needs review. Successful helper output establishes its observed preservation/history checks. Operations still verifies active bindings, receiver readiness, no historical turn after core bootstrap, and a separately authorized real human input → stored inbound → Processor run → visible reply. Full human E2E and no-replay runtime evidence remain unverified until that observation. The helper sends no synthetic Twitch messages.

Isolated verification:

```sh
python3 -m py_compile deploy/production/migrate_identity.py deploy/production/test_migrate_identity.py
python3 -m unittest discover -s deploy/production -p 'test_*.py' -q
```

The in-memory graph/Engine fixtures cover response loss after committed mutations, inverse restoration, staging, payload boundaries, stopped-writer guards, exact environment preservation, unknown creation and drain-probe refusal. They perform no production/network/credential access. Design/source/runtime contracts: DiVoid #32582/#32583/#32584/#32586.
