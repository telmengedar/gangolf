# Legacy API archive operator runbook

This Python 3.9+ stdlib helper consumes Operations' immutable
`gangolf-legacy-operational-capture-v1` capture. It does not export credentials,
replay messages, call models, change active configuration, or stop services.
The capture contains raw chat/profile data: keep capture, plans and journal in
owned 0700 directories with 0600 files. Never paste their contents into logs,
shared DiVoid or tickets. Console output contains aggregate receipts only.

Operations owns capture and live import. Capture must have `manifest.json`,
`manifest.sha256`, dataset page files, and each dataset's `multiset.json`.
The helper checks manifest/page bytes and hashes, every pagination count and
continuation, and every canonical record multiset. Equal live captures do not
prove a consistent database snapshot: the old API uses unordered pagination.
Original account IDs, replies, orphan rows and timestamp timezone cannot be
recovered from that projection. Naive original times retain all digits unchanged.
Clip preservation means video/trigger metadata, not downloaded media.

Use the existing principal4 credential file with `User`, `UserId`, `Key`
assignments; values are split at the first equals sign. The helper independently
requires enabled read/write principal4 from `/users/me` and only accepts
`http://gangolf:8091/api`. It creates a deterministic separate private archive
project; every node skips embeddings. No archive node has canonical `message`
type or membership of live Messages4. Service isolation still needs Operations'
no-response observations during import.

From the repository root, substitute the protected paths below:

```sh
python3 deploy/production/archive_legacy.py plan \
  --capture /protected/frozen-capture --pilot-limit 100 \
  --plan /protected/work/pilot-plan.json
python3 deploy/production/archive_legacy.py import \
  --capture /protected/frozen-capture --plan /protected/work/pilot-plan.json \
  --journal /protected/work/archive-journal.json \
  --credential /home/toni/.codex/secrets/.gangolf-divoid-gangolf
python3 deploy/production/archive_legacy.py verify \
  --capture /protected/frozen-capture --plan /protected/work/pilot-plan.json \
  --journal /protected/work/archive-journal.json \
  --credential /home/toni/.codex/secrets/.gangolf-divoid-gangolf
```

Pilot limits only message records; all captured channels/videos/triggers and
observed identities needed by those messages are included. Before/during/after
pilot, Operations measures disk growth, duration, API/core health and latency,
and absence of historical responses/intents. The receipt counts actual HTTP
calls and mutations. Project full cost from measured messages plus fixed media,
identity and group overhead; do not equate message count with HTTP call count.
Full plan currently needs memory for raw capture, plan and target inventory;
its serialized size is also reported by offline planning measurements.

After measured storage/load acceptance, create a separate full plan by omitting
`--pilot-limit`. Import it with the same capture, credential and journal, plus
`--full-import-authorized`. This flag is the operator's attestation of the
volume gate; the helper cannot observe service load itself. Pilot nodes are
reused in the full archive. A complete full import must pass `verify` against
the full plan, then rerunning `import` must report `changes: {}`. Full verify
checks every expected key, raw payload/digest and incident edge, including
omissions, extras, wrong privacy/owner/type/root and dangling unexpected links.
Pilot verify is not full acceptance and will refuse extra full-import nodes.

Each source row retains raw fields including null versus absent and arbitrary
chunk types. Identical messages have distinct digest occurrence keys in this
snapshot. A new capture is a separate namespace; never combine captures by
message text/time. Observed service/name authors remain unresolved original
accounts. Unique catalog channel matches have in-channel edges. Only resolvable
numeric trigger video FKs produce references-video links; missing FKs remain
explicitly unresolved in retained content. No reply links are invented.

Use one operator on one host. A per-user host lock serializes helper processes;
it does not exclude another host or manual graph writers. Journal writes are
constant size, with a durable pending metadata intent before each creation;
target graph readback is truth. Existing exact nodes/content/links are reused.
Missing staged content/links can be completed by rerunning the same invocation.
Metadata or nonempty-content conflicts stop without overwriting them.

An unknown create outcome triggers deterministic-name readback. One match is
reconciled; zero or multiple matches stop, with the pending intent retained.
Do not delete/reset the journal or blindly retry POST: Operations must resolve
visibility/ambiguity and resume only once the original intended node can be
uniquely read. Content/link transport failures also require readback; unresolved
ones stop and can be reconciled on rerun. No automatic deletion/rollback exists.
Partial archives stay isolated; preserve them and the frozen capture for recovery.
The journal phase becomes `verified` only after complete verification of its
current plan. The archive manifest describes captured counts and limitations;
it is not a completion marker.

Run synthetic checks without credentials or network:

```sh
python3 -m unittest discover -s deploy/production -p test_archive_legacy.py -v
```
