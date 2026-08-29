-- Immutable, content-addressed recommendation authority records (plan §4).
-- The foreign key is deferred so a producer may persist a source immediately
-- before its new item in the same transaction, allowing PutAttentionItem to
-- derive the recommendation at creation.
CREATE TABLE attention_recommendation_sources (
    item_id                 TEXT NOT NULL REFERENCES attention_items (id)
                                 DEFERRABLE INITIALLY DEFERRED,
    digest                  TEXT NOT NULL PRIMARY KEY,
    source                  TEXT NOT NULL CHECK (source IN (
                                'daemon_policy', 'agent_judgment', 'project_policy'
                            )),
    decision_surface_digest TEXT NOT NULL CHECK (decision_surface_digest <> ''),
    body                    TEXT NOT NULL CHECK (body <> '')
) STRICT;
