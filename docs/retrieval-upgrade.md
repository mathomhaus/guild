# Retrieval index upgrade

This release repairs quest search identity to use the project and quest number
together. Migration 014 reconstructs searchable text from project-scoped quest
notes and discards existing quest vectors, which may contain text from multiple
projects. Quest records and notes are preserved. Semantic search becomes usable
again as fresh vectors are rebuilt; keyword search remains available.

Before the first startup of the new binary:

1. Stop every Guild MCP server and the Guild daemon. Close their client
   connections so older binaries cannot continue serving searches or backfills.
2. With Guild stopped, back up the Guild data directory, including both SQLite
   databases and any SQLite journal or WAL files.
3. Install the new binary and restart the daemon and MCP server connections
   using it. The first new startup applies the migration automatically.

A restart is required for every running server. Older binaries assemble quest
text using the quest number alone and can write mixed project vectors into the
upgraded schema because the embedding model identity has not changed. Their
search code also lacks the new project filters. Installing a new executable does
not replace code already running in memory.

Do not run an older binary against databases after migration 014. To roll back,
stop all Guild processes and restore the matching pre-upgrade database backup
before restarting the older version. Keep the upgraded data and the backup
separate until the upgrade has been verified.
