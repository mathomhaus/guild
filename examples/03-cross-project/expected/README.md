# Expected output

Captured snapshots from a reference run of this example. The entry IDs (LORE-210, LORE-211)
will differ in your run — guild assigns IDs globally. The shape of the output is what matters.

- `project-a-lore.txt` — project A's single pre-seeded entry (`guild lore list --project guild-example-03-project-a`)
- `project-b-lore.txt` — project B's new entry after the agent inscribes it (`guild lore list --project guild-example-03-project-b`)
- `informs-edge.txt` — full study output of B's entry, showing the `informs` edge pointing back to A's entry (`guild lore study <B-entry-id>`)

The `LINKED ENTRIES` block in `informs-edge.txt` is the key artifact: it proves guild's lore graph
spans project boundaries. The `←` arrow means "this entry was informed by" the linked entry in
project A.
