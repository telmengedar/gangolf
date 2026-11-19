# Restore Gangolf's own memory

`import_memory.py` is a Python 3.9+ standard-library migration helper for the
bounded restoration in DiVoid #34389. It copies a frozen, independently reviewed
thought/knowledge manifest from the qualified source into the actual Gangolf
instance. Operations owns capture, credentials, execution and independent runtime
readbacks; the scripting owner maintains this tool. This tool changes no service,
production configuration, source note or pre-existing target thought.

The REST compatibility contract is #34405 at DiVoid source
`4acc687f63b94c399c8cfb93b7c9cf1de788eecd`. Installed behavior and embedding
readiness still require Operations evidence.

## Inputs and review

Keep every capture, plan, credential stream and journal outside the checkout.
Inputs must be owned regular files with mode `0600`, without symlinks or hard
links. Outputs require an owned directory with no group/other permission bits.
Use one journal for one reviewed plan; retain both as the restoration backup.

Operations supplies a capture with `sourceOrigin`, `sourcePrincipal`,
`targetOrigin`, `targetPrincipal`, `mapping` (`901→8`, `902→1`), `nodes`,
`edges`, `omittedEdges` and `excluded`. Each node contains original `metadata`,
exact `bodyBase64` or null, `bodySha256` or null, and `membershipProvenance`.
The separate baseline contains metadata, exact bodies and complete incident
links for the existing controls, including the seven thoughts and anchors 1/8.
Hashes below are SHA-256 of the exact input file bytes.

```sh
python3 import_memory.py prepare \
  --manifest /private/source-capture.json --manifest-sha256 CAPTURE_SHA256 \
  --baseline /private/target-baseline.json --baseline-sha256 BASELINE_SHA256 \
  --output /private/review-plan.json
```

Preparation performs no HTTP operations. It validates closed membership/edges,
retains omitted topology and source provenance, normalizes absent nullable
metadata fields, and prints the generated plan's hash and counts. Root and
independent QA review this exact plan and the helper before any apply. Preparing
a plan does not constitute approval.

```sh
python3 import_memory.py validate \
  --manifest /private/review-plan.json --manifest-sha256 REVIEWED_PLAN_SHA256
```

## Execution and recovery

`apply`, `verify` and `rollback` use the same plan/hash and `--journal` path.
They read a JSON object with `sourceKey` and `targetKey` from stdin. Operations
must provide that stream privately, without shell tracing or printing it. Tokens
never appear in command arguments, environment variables or tool output. The
source transport enforces GET-only, and both origins and principal IDs are pinned.
Redirects and proxies are disabled.

```sh
python3 import_memory.py apply \
  --manifest /private/review-plan.json --manifest-sha256 REVIEWED_PLAN_SHA256 \
  --journal /private/import-journal.json < /private/runtime-credentials.json
```

Before writes, the helper checks source bodies/metadata/all incident links,
the seven original thoughts exactly, and the two mapped anchors. Anchor bodies
and metadata remain exact except for generated update timestamps; their original
links must remain, and only planned additions are allowed. A per-journal process
lock prevents overlapping invocations. A durable private journal is saved before
each create/delete attempt and after observed identity/stage transitions. Progress
contains stage names and counts only; HTTP bodies and exceptions are never logged.

The write sequence is:

1. Create a new private owner-3/root-1 node with a unique temporary name and
   `skipEmbedding=true`; save its returned ID and native creation timestamp.
2. Upload exact raw bytes/MIME only when the source has a blob, then restore exact
   substance and final name. Null, empty text, empty blob and absent blob remain
   distinct. Source status, type, severity and refinement are preserved.
3. Create only closed mapped links, retaining stored orientation/type/context.
4. Restore final coordinates after link side effects and restore the original
   `skipEmbedding` value. This requests one synchronous embedding activation for
   each originally eligible node. The helper verifies the resulting fields and
   bytes; Operations separately qualifies actual search readiness.

Native target IDs/owner/root/privacy and creation/update timestamps differ by
design. Original IDs, owner/access, timestamps, substance, eligibility, content
hashes and membership evidence remain in the reviewed plan and private journal.
Distinct source IDs always get distinct target IDs, even when their text matches.

A repeated successful apply verifies and performs no writes. An interrupted apply
can resume from the same journal. An ambiguous create is **never replayed**: the
helper searches for its exact temporary marker and accepts only one matching
private node. Zero or multiple matches stop for operator reconciliation. Do not
delete the journal or create a replacement journal to bypass this stop.
Content/name/activation uncertainty is reconciled by readback; exact content is
not uploaded again when already present. A changed body or unrelated metadata
stops the run. Existing link pairs are idempotent but their labels are verified.

Use `verify` for a fresh source-preservation, existing-seven, mapping, exact-byte,
metadata and complete imported-link check. It does not call the inference model
or assert provider/search readiness.

Use `rollback` only within the authorized restoration and with its original
reviewed plan/journal. It deletes only the exact newly created IDs after checking
owner/root/privacy, native creation identity, expected metadata/content and links.
It refuses changed substantive data. During unfinished linking, generated
coordinate movement is allowed; finalized nodes require exact coordinates.
An uncertain delete is reconciled by readback. The seven original thoughts and
source never receive writes. A rolled-back journal cannot be applied again.

## Offline checks

```sh
python3 -m unittest -v test_import_memory.py
python3 import_memory.py --help
```

The fake HTTP boundary covers exact UTF-8/CRLF and absent/empty content,
substance clearing, orphan positioning, private mapping, original provenance,
eligibility staging, repeat apply, cancellation, partial/uncertain create/content/
link/activation/delete outcomes, drift refusal, rollback ownership and error
redaction. These are local behavioral checks; they do not establish real import
completion, live ACL visibility or actual embedding/search readiness.
