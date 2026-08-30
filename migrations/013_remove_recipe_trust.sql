-- Recipe manifests are launchable after validation; persisted operator trust is obsolete.
ALTER TABLE recipes DROP COLUMN trust_state;
