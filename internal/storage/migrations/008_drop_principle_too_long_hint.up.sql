-- 008: drop the retired `principle-too-long` hint row.
--
-- The hints-engine rule was removed (see #52); the inline ⚠️ warning emitted
-- by lore_inscribe already covers the same condition with entry-specific
-- context (word count + remedy command). Without this migration, fresh and
-- upgraded databases keep an enabled row that surfaces in `guild hints list`
-- and `guild hints stats` but can never fire because the rule isn't in
-- internal/hints/rules.go::Definitions().

DELETE FROM hints WHERE rule_id = 'principle-too-long';
