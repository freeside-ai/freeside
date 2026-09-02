PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    digest     TEXT NOT NULL,
    applied_at TEXT NOT NULL
) STRICT;
INSERT INTO schema_migrations VALUES(1,'0001_server_state.sql','sha256:d35e24aecc4be5d20a2426e7ce809ed2959b97c34991da4b894aa2efdc1720b9','2026-09-02T04:05:16.811741Z');
INSERT INTO schema_migrations VALUES(2,'0002_domain.sql','sha256:be58a20ad732312a26083a03ccc8cb01197b69c48c07abaf5e9b7e25d0f3a90a','2026-09-02T04:05:16.813311Z');
INSERT INTO schema_migrations VALUES(3,'0003_inbox_outbox.sql','sha256:d128b364d46343b7fb9d2dc99f7641a4c1215677c4d97fbb2a20f0a1578b6303','2026-09-02T04:05:16.81431Z');
INSERT INTO schema_migrations VALUES(4,'0004_commands.sql','sha256:bc108c2fe12984124b6c277fb7c8688bb1493cb308c4edac488f04d4d83285ee','2026-09-02T04:05:16.814878Z');
INSERT INTO schema_migrations VALUES(5,'0005_devices.sql','sha256:0e9d39becfe3953bfe8a6726b7e991f6ac9924712105253e6b9bb6f86a00b0c0','2026-09-02T04:05:16.815564Z');
INSERT INTO schema_migrations VALUES(6,'0006_publish_mint_audit.sql','sha256:8b4bd4b58dfc8a8909bd74a46a5e889db01579fab0bdd1566c6ee3c33960d421','2026-09-02T04:05:16.816097Z');
INSERT INTO schema_migrations VALUES(7,'0007_trust_authorization.sql','sha256:51e1cdc284f6c4b84dc9aaf9be2d832c414ea9261e9586a21d67e9ba49ec1949','2026-09-02T04:05:16.816806Z');
INSERT INTO schema_migrations VALUES(8,'0008_trust_profile_activations.sql','sha256:3781ff6fe573110626d68ffb37f02fdbce4c31f4af015d4130cc521c83c72660','2026-09-02T04:05:16.817475Z');
INSERT INTO schema_migrations VALUES(9,'0009_publish_audit_permissions.sql','sha256:c635b1eb003bacf9227578bf86e819f3765e94166de3295cfa932d516dd02442','2026-09-02T04:05:16.821155Z');
INSERT INTO schema_migrations VALUES(10,'0010_publish_audit_registration.sql','sha256:9fc250c7eada246daab38f502430851391c56b826982bee67af4aabf212fb83d','2026-09-02T04:05:16.821931Z');
INSERT INTO schema_migrations VALUES(11,'0011_publish_audit_repository.sql','sha256:e6b559959d30bb8b28f564a04c55524370d786cef4697e4ae60158bcb5b3e453','2026-09-02T04:05:16.822639Z');
INSERT INTO schema_migrations VALUES(12,'0012_candidate_authorization_v2.sql','sha256:730bdb7d873d8a6dcab9a755781725c6202603634934752dec42a8fb86166f35','2026-09-02T04:05:16.823149Z');
INSERT INTO schema_migrations VALUES(13,'0013_auth_identity.sql','sha256:8dbc7b80ac04401e841618470b229c671c58973e35d53b667698e03c347b1a6a','2026-09-02T04:05:16.823689Z');
INSERT INTO schema_migrations VALUES(14,'0014_execution_records.sql','sha256:77b270e9b44ed9d1dea4259334a739410e776bf0c5016666ff09068ffa04420b','2026-09-02T04:05:16.824367Z');
INSERT INTO schema_migrations VALUES(15,'0015_command_backup_binding.sql','sha256:fd43c93144b79ebc0cd879d5376b7559a02ab0f8a913cde4637ea64e8e923865','2026-09-02T04:05:16.825511Z');
INSERT INTO schema_migrations VALUES(16,'0016_project_images.sql','sha256:0d5c3fdcf480ee66e0eeedd84a498a730bd45744902db769916f2f0eb6de1c26','2026-09-02T04:05:16.825998Z');
INSERT INTO schema_migrations VALUES(17,'0017_unattended_operation.sql','sha256:e80a10864e98c7ae689c773fae16ad8dcfa7ece354a59c1304167732d07d3ba9','2026-09-02T04:05:16.827777Z');
INSERT INTO schema_migrations VALUES(18,'0018_backend_conformance.sql','sha256:d4cf0a4f1c8b3a2d3e68923a73e2fc8d737a35ae6d1d9c4f9f644f4cd2cdd01c','2026-09-02T04:05:16.828206Z');
INSERT INTO schema_migrations VALUES(19,'0019_handoff_journal.sql','sha256:e09607ff5ee5e5feee4ed6b97d22cc8eeb3feb904cd3600b2ae4952958a811df','2026-09-02T04:05:16.829364Z');
INSERT INTO schema_migrations VALUES(20,'0020_backend_conformance_configuration.sql','sha256:defdc9d1b26391786c609bcd16dfd46d4370f03bf45ffd71aee0d1a798736ca9','2026-09-02T04:05:16.830407Z');
INSERT INTO schema_migrations VALUES(21,'0021_execution_outcomes.sql','sha256:633ecc721761c208e8f14985fbd073580c5a247994d924bd4031ec4deb7067da','2026-09-02T04:05:16.830941Z');
INSERT INTO schema_migrations VALUES(22,'0022_handoff_failed_outcome.sql','sha256:39e66124e4ffe3cc91d2c6fb01937bfcf5500700c416959c76921f8f56dee4b0','2026-09-02T04:05:16.833708Z');
INSERT INTO schema_migrations VALUES(23,'0023_workflow_audit_evidence.sql','sha256:009f5346e7eba5670cc744ab91f2b19f7f43ab305d476cab474a3a4ed67d303a','2026-09-02T04:05:16.836503Z');
INSERT INTO schema_migrations VALUES(24,'0024_run_observation.sql','sha256:d3639b65ee35f6f5d08b549e3fb2736ea55e30754abac4c20733dc0d30c1a0bb','2026-09-02T04:05:16.837284Z');
INSERT INTO schema_migrations VALUES(25,'0025_schedule.sql','sha256:ea074747dd3eb53dcc57157fbbfa83c6e1a2952a2e8bef784a4a4929e78b1c36','2026-09-02T04:05:16.838243Z');
INSERT INTO schema_migrations VALUES(26,'0026_work_unit_capture.sql','sha256:cc84bc2269434a83e89ce14597656856e63216fd69f3601900e9d047ef517987','2026-09-02T04:05:16.839144Z');
INSERT INTO schema_migrations VALUES(27,'0027_schedule_authority.sql','sha256:2dbb493b6bd7b8720d2e297e99bfdb074e83db3b094ca3870a94a989095a8171','2026-09-02T04:05:16.844067Z');
INSERT INTO schema_migrations VALUES(28,'0028_ready_item_pr_binding.sql','sha256:dbdafec213a855b6c64e0ceddd61047b9f4e4c9e52a9e2f62dd0cf1beb78347c','2026-09-02T04:05:16.844795Z');
INSERT INTO schema_migrations VALUES(29,'0029_review_stage.sql','sha256:98fbf046d7d915dc165161a4f176bc241bc589b2b41d6b3057e9854c4fdcbc61','2026-09-02T04:05:16.846056Z');
INSERT INTO schema_migrations VALUES(30,'0030_native_review_observation.sql','sha256:963ed0b7948eec370ecfe5966aed7138e7b98ae64059851ca333e1ec67c6337e','2026-09-02T04:05:16.846793Z');
INSERT INTO schema_migrations VALUES(31,'0031_review_retry.sql','sha256:ac5ab37c4bb266abd1b71538412df942f043480eedefcf12217cb5fd9a49ba17','2026-09-02T04:05:16.847115Z');
INSERT INTO schema_migrations VALUES(32,'0032_review_recovery.sql','sha256:e40391dd5f2b5cdddbec370ba0ee021bd16603d3844bd5f96e777ce93caa8ac5','2026-09-02T04:05:16.847562Z');
INSERT INTO schema_migrations VALUES(33,'0033_publish_installation_mint_audit.sql','sha256:c8dc3e1d9d68fb343123cdf332cd0bcd8e8cbc819f73e685eb9f7d95a8113447','2026-09-02T04:05:16.847901Z');
INSERT INTO schema_migrations VALUES(34,'0034_review_configuration_recovery.sql','sha256:290d922986d9fedc61d3d4634c7a247e5b418a737cc47f2ce8d3eeb6740a51ac','2026-09-02T04:05:16.848311Z');
INSERT INTO schema_migrations VALUES(35,'0035_attention_health_posture.sql','sha256:ab0b6a142dba7150302ad7ad801690d5bf3be609e0a46c62542be59902c125ca','2026-09-02T04:05:16.849646Z');
INSERT INTO schema_migrations VALUES(36,'0036_attention_pr_reference.sql','sha256:0564a7c130ef209b75fe64475dc7f16137802633f28ba25a1ea9e12072e11c14','2026-09-02T04:05:16.850034Z');
INSERT INTO schema_migrations VALUES(37,'0037_finding_dispositions.sql','sha256:c7b5d998d2de22c30d8a31c7b0175f4996b6a37ff180e574b69c5ae3ab261480','2026-09-02T04:05:16.850604Z');
INSERT INTO schema_migrations VALUES(38,'0038_verification_readiness.sql','sha256:d3d8ac07e58144a5bf6552b31180dee6fcc29d2a573bfff370fee64516db4156','2026-09-02T04:05:16.8513Z');
INSERT INTO schema_migrations VALUES(39,'0039_outbox_payload_authentication.sql','sha256:ac341c5ab4d7dc61eff323b184145b2e63d482993ec5b17ee98651a406513d95','2026-09-02T04:05:16.854282Z');
INSERT INTO schema_migrations VALUES(40,'0040_codex_reenrollment.sql','sha256:c9f6b5d18a3bbc1cafd8a82f47bcf2cb07958f5668f1a42075c53674aad432bd','2026-09-02T04:05:16.854952Z');
INSERT INTO schema_migrations VALUES(41,'0041_effect_proposals.sql','sha256:8059d041f4831e62fab774979bc966b3a55397c51b6e5b61ea560b0993fc37db','2026-09-02T04:05:16.855874Z');
INSERT INTO schema_migrations VALUES(42,'0042_execution_identity_parallelism.sql','sha256:7c2719cb5a171bbe8e8b861ddef9fbf3a2a065c3fd6e0e61d47fe2da53398bf4','2026-09-02T04:05:16.856295Z');
INSERT INTO schema_migrations VALUES(43,'0043_intake_occurrences.sql','sha256:bbb06309e392fc5dccc24a037ac8b0bd2261a92186ed1eb0233fdcda1df6749c','2026-09-02T04:05:16.856712Z');
INSERT INTO schema_migrations VALUES(44,'0044_agent_claims.sql','sha256:14aea2377608c50a6691a1f7609f912afb5f0784c4a0d82bcd83ba2b83d55044','2026-09-02T04:05:16.856973Z');
INSERT INTO schema_migrations VALUES(45,'0045_projects.sql','sha256:e27c8c7ebf6bde0831e6f060a797171abb33e512b84260b0f9209bec08b1ab10','2026-09-02T04:05:16.857206Z');
INSERT INTO schema_migrations VALUES(46,'0046_export_rejections.sql','sha256:ada2e73f752aeb96f56418ba5f65ef92a3729fb8e29285e82040131278aab4ba','2026-09-02T04:05:16.857454Z');
INSERT INTO schema_migrations VALUES(47,'0047_current_import_starts.sql','sha256:179a089dbbfcfbe3d71017d7cab013437ce82fe74e9d0995db207b05c05cafe4','2026-09-02T04:05:16.857687Z');
INSERT INTO schema_migrations VALUES(48,'0048_production_attempts.sql','sha256:b52127235e66ba4ed12dd56e8b329a72a7c9481014ed25583a1034bc9da7974b','2026-09-02T04:05:16.863341Z');
INSERT INTO schema_migrations VALUES(49,'0049_production_attempt_publication_digest.sql','sha256:fa2c642dfc5e4d006fb465d104e41602d9cbdef2757cf3c658b61e0288af1284','2026-09-02T04:05:16.865103Z');
INSERT INTO schema_migrations VALUES(50,'0050_attention_subject_run_binding.sql','sha256:2a0a5983e82b4e735066c8a909d614001cc4949cd9ace3c1f782225aa474f642','2026-09-02T04:05:16.866608Z');
INSERT INTO schema_migrations VALUES(51,'0051_finding_adjudications.sql','sha256:66ef33e2fe5fd4f46d6438d3763657efe282444fdbfe3cd2bfd4a974c43ce6c6','2026-09-02T04:05:16.866921Z');
INSERT INTO schema_migrations VALUES(52,'0052_admitted_agents.sql','sha256:ce2dfc207c5f2946092a5fa44e43aa42bc9f2ef41e2b20ba82b7aafebbe75109','2026-09-02T04:05:16.877641Z');
INSERT INTO schema_migrations VALUES(53,'0053_shadow_review.sql','sha256:741936393061a1235fe3046b5dfeb054420f27aadba84785f55131ef93391a4e','2026-09-02T04:05:16.879243Z');
INSERT INTO schema_migrations VALUES(54,'0054_attention_readiness_summary.sql','sha256:f10c056e7ccd33ec8be122ba83a3c12f5eb9601eb02b653efc87e4d636063e14','2026-09-02T04:05:16.88108Z');
INSERT INTO schema_migrations VALUES(55,'0055_attention_yield_history.sql','sha256:968767ec44298d13db9722b0df52c043f0556f82b3bdd49f45fd2fa69138c638','2026-09-02T04:05:16.882507Z');
INSERT INTO schema_migrations VALUES(56,'0056_shadow_review_configuration_approval.sql','sha256:107039906b2b696a95b07d9141db7050972157c1b369b82bbc06be06b40e3c9a','2026-09-02T04:05:16.882977Z');
INSERT INTO schema_migrations VALUES(57,'0057_finding_adjudication_revisions.sql','sha256:9e4d5df7f84c61e56b153e33b96c32ec680179f3138f840bc4284cffa47569ab','2026-09-02T04:05:16.88758Z');
INSERT INTO schema_migrations VALUES(58,'0058_attention_decision_surfaces.sql','sha256:5784f3d46da0f524109039f1ad04b13b9e9a8edc1f052b5388e3eccd46694d46','2026-09-02T04:05:16.887868Z');
INSERT INTO schema_migrations VALUES(59,'0059_attention_decision_surface_bodies.sql','sha256:4da39273d1edfc3656f908a34f827ac4fbc19c8c5e3faf56e3905e324ef8dc8e','2026-09-02T04:05:16.888008Z');
INSERT INTO schema_migrations VALUES(60,'0060_attention_recommendation_sources.sql','sha256:d50f91856b67c10dc2ecf707efd1c60e42badb36917c55eccd08a458c72d545d','2026-09-02T04:05:16.888195Z');
INSERT INTO schema_migrations VALUES(61,'0061_usage_observations.sql','sha256:a2d1b6e7207595561d6d46afb9f7daee8d16955f44ecf584fad211a6a8baa35b','2026-09-02T04:05:16.888649Z');
INSERT INTO schema_migrations VALUES(62,'0062_ready_return_action.sql','sha256:21af970668d892c017788a8e5600621c15f6583a33bab70f365d1b18d4b3b7ff','2026-09-02T04:05:16.888872Z');
INSERT INTO schema_migrations VALUES(63,'0063_attention_readiness_detail.sql','sha256:3c100c328988b9ee1e4c31d93d115b76ade27e6161b65e779d02e81743ed996f','2026-09-02T04:05:16.890487Z');
CREATE TABLE server_state (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    sync_epoch TEXT    NOT NULL,
    revision   INTEGER NOT NULL
) STRICT;
INSERT INTO server_state VALUES(1,'033d458f644b70ce320d6af5726312a3',17);
CREATE TABLE runs (
    id             TEXT PRIMARY KEY,
    project_id     TEXT    NOT NULL,
    policy_digest  TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
, campaign_id TEXT, attempt_number INTEGER, attempt_reason TEXT, parent_run_id TEXT REFERENCES runs (id)) STRICT;
INSERT INTO runs VALUES('run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','project-submit-elaboration','sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9',3,12,'{"id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","project_id":"project-submit-elaboration","spec_digest":"sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","policy_digest":"sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","campaign_id":"campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba","attempt_number":1,"stages":[{"id":"elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","run_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","name":"elaboration","attempts":[{"id":"attempt-inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","stage_id":"elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","number":1,"invocation_id":"inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1"},{"id":"attempt-elaboration-discussion-explain-submitted-spec","stage_id":"elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","number":2,"invocation_id":"elaboration-discussion-explain-submitted-spec"}]}]}','campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba',1,NULL,NULL);
INSERT INTO runs VALUES('implementation-from-submit','project-submit-elaboration','sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9',1,11,'{"id":"implementation-from-submit","project_id":"project-submit-elaboration","spec_digest":"sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","policy_digest":"sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","campaign_id":"campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba","attempt_number":1,"stages":[{"id":"implement-implementation-from-submit","run_id":"implementation-from-submit","name":"implement","attempts":[]}]}','campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba',1,NULL,NULL);
CREATE TABLE conversations (
    id             TEXT PRIMARY KEY,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;
INSERT INTO conversations VALUES('conv-spec-approval-implementation-from-submit-1',2,16,'{"id":"conv-spec-approval-implementation-from-submit-1","status":"idle","messages":[{"id":"msg-user-explain-submitted-spec","conversation_id":"conv-spec-approval-implementation-from-submit-1","sequence":1,"author":"user","body":"Why does the specification keep the workflow bounded?","attachments":[],"created_at":"2026-09-02T04:05:16Z"},{"id":"msg-agent-inv-explain-submitted-spec","conversation_id":"conv-spec-approval-implementation-from-submit-1","sequence":2,"author":"agent","body":"It keeps the workflow bounded by pinning the approved artifact and declared scope.","attachments":[],"created_at":"2026-09-02T04:05:16Z"}]}');
CREATE TABLE agent_invocations (
    id             TEXT PRIMARY KEY,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;
INSERT INTO agent_invocations VALUES('inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1',1,2,'{"id":"inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","input_ids":["artifact-specification-f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403"],"conversation_id":null,"through_sequence":0}');
INSERT INTO agent_invocations VALUES('inv-explain-submitted-spec',1,8,'{"id":"inv-explain-submitted-spec","input_ids":[],"conversation_id":"conv-spec-approval-implementation-from-submit-1","through_sequence":1}');
INSERT INTO agent_invocations VALUES('elaboration-discussion-explain-submitted-spec',1,10,'{"id":"elaboration-discussion-explain-submitted-spec","input_ids":["artifact-specification-f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","spec-implementation-from-submit-1","spec-discussion-explain-submitted-spec"],"conversation_id":"conv-spec-approval-implementation-from-submit-1","through_sequence":1}');
INSERT INTO agent_invocations VALUES('inv-implement-implementation-from-submit',1,11,'{"id":"inv-implement-implementation-from-submit","input_ids":["spec-implementation-from-submit-1"],"conversation_id":null,"through_sequence":0}');
CREATE TABLE artifacts (
    id             TEXT PRIMARY KEY,
    digest         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;
INSERT INTO artifacts VALUES('artifact-specification-f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403','sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403',1,1,'{"id":"artifact-specification-f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","type":"specification","digest":"sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","provenance":{"producer_class":"daemon","producer_invocation_id":"submit-specification-f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","head_binding":"head_independent","verification_recipe_digest":null,"sensitivity_class":"normal"},"publish_eligible":false,"metadata":{"media_type":"text/markdown","size_bytes":33,"created_at":"2026-09-02T04:05:16.927513Z","source":"run","availability":"available"}}');
INSERT INTO artifacts VALUES('artifact-policy-5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9','sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9',1,1,'{"id":"artifact-policy-5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","type":"policy","digest":"sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","provenance":{"producer_class":"daemon","producer_invocation_id":"submit-policy-5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","head_binding":"head_independent","verification_recipe_digest":null,"sensitivity_class":"normal"},"publish_eligible":false,"metadata":{"media_type":"application/json","size_bytes":1479,"created_at":"2026-09-02T04:05:16.927546Z","source":"run","availability":"available"}}');
INSERT INTO artifacts VALUES('spec-implementation-from-submit-1','sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2',1,7,'{"id":"spec-implementation-from-submit-1","type":"specification","digest":"sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","provenance":{"producer_class":"agent","producer_invocation_id":"inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","head_binding":"head_independent","verification_recipe_digest":null,"sensitivity_class":"normal"},"publish_eligible":false,"metadata":{"media_type":"text/markdown","size_bytes":72,"created_at":"2026-09-02T04:05:16Z","source":"run","availability":"available"}}');
INSERT INTO artifacts VALUES('spec-discussion-explain-submitted-spec','sha256:9740e7993930e8bf594dee86bbd75b7bb57b395b1236ce3eb16970ed4a6d74c8',1,10,'{"id":"spec-discussion-explain-submitted-spec","type":"research","digest":"sha256:9740e7993930e8bf594dee86bbd75b7bb57b395b1236ce3eb16970ed4a6d74c8","provenance":{"producer_class":"daemon","producer_invocation_id":"inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","head_binding":"head_independent","verification_recipe_digest":null,"sensitivity_class":"normal"},"publish_eligible":false,"metadata":{"media_type":"application/json","size_bytes":401,"created_at":"2026-09-02T04:05:16Z","source":"run","availability":"available"}}');
CREATE TABLE attention_items (
    id              TEXT PRIMARY KEY,
    project_id      TEXT    NOT NULL,
    conversation_id TEXT    REFERENCES conversations (id),
    entity_version  INTEGER NOT NULL,
    as_of_revision  INTEGER NOT NULL,
    body            TEXT    NOT NULL
, item_type TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', health_posture TEXT
    CHECK (health_posture IS NULL OR health_posture IN ('blocking', 'advisory')), subject_run_id TEXT, readiness_summary TEXT
CHECK (readiness_summary IS NULL OR readiness_summary <> ''), yield_history TEXT
CHECK (yield_history IS NULL OR yield_history <> ''), readiness_detail TEXT
CHECK (readiness_detail IS NULL OR readiness_detail <> '')) STRICT;
INSERT INTO attention_items VALUES('spec-approval-implementation-from-submit-1','project-submit-elaboration','conv-spec-approval-implementation-from-submit-1',3,9,'{"id":"spec-approval-implementation-from-submit-1","project_id":"project-submit-elaboration","subject":{"subject_type":"run","subject_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","run_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160"},"type":"spec_approval","priority":"normal","reason":"Ready for implementation.","requested_decision":["approve","request_changes","discuss","stop"],"recommendation":null,"decision_surface":{"epoch":1,"digest":"sha256:1da37365a32af17b5fd4eebcb60ee5de5b318f93b5f11e53acec679bb10d6b94"},"evidence_snapshot":null,"agent_claims":[{"label":"Specification","artifact_id":"spec-implementation-from-submit-1","digest":"sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","provenance":{"producer_class":"agent","producer_invocation_id":"inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","head_binding":"head_independent","verification_recipe_digest":null,"sensitivity_class":"normal"},"text":{"media_type":"text/markdown","content":"# Approved specification\n\nImplement the thing within the declared paths."},"metadata":{"media_type":"text/markdown","size_bytes":72,"created_at":"2026-09-02T04:05:16Z","source":"claim","availability":"available"}},{"label":"freeside.summary","artifact_id":"spec-summary-implementation-from-submit-1","digest":"sha256:8e714f53a1974d5baffab99aff7e83fcd8f96d5ffc36c13b73fea0bed9b16dfe","provenance":{"producer_class":"agent","producer_invocation_id":"inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","head_binding":"head_independent","verification_recipe_digest":null,"sensitivity_class":"normal"},"text":{"media_type":"text/markdown","content":"Ready for implementation."},"metadata":{"media_type":"text/markdown","size_bytes":25,"created_at":"2026-09-02T04:05:16Z","source":"claim","availability":"available"}}],"artifact_digests":["sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","sha256:8e714f53a1974d5baffab99aff7e83fcd8f96d5ffc36c13b73fea0bed9b16dfe"],"pr_head_sha":"","pr_reference":null,"readiness":null,"readiness_detail":null,"yield_history":null,"commit_plan_notice":null,"base_freshness":null,"readiness_invalidation":null,"review_recovery_binding":null,"codex_reenrollment_recovery_binding":null,"review_configuration_recovery":null,"finding_adjudication":null,"display_names":{"project":{"text":"project-submit-elaboration","source":"identifier"},"work_unit":{"text":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","source":"identifier"}},"billable_cost_so_far":null,"execution_failure":null,"publish_block":null,"diff_stats":null,"blocked_on":null,"health_diagnostic":null,"review_dispute":null,"spec_revision":null,"item_version":3,"interruption_class":"planned_gate","conversation_id":"conv-spec-approval-implementation-from-submit-1","timing":{"delivery_count":0,"first_submitted_at":null,"first_accepted_at":null,"first_opened_at":null,"submit_to_first_open":null},"created_at":"2026-09-02T04:05:16Z","expires_when":null,"decided_at":"2026-09-02T04:05:16Z","posture":null,"blocking_supersession":null,"status":"resolved"}','spec_approval','resolved',NULL,'run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160',NULL,NULL,NULL);
CREATE TABLE attention_deliveries (
    item_id        TEXT    NOT NULL REFERENCES attention_items (id),
    device_id      TEXT    NOT NULL,
    channel        TEXT    NOT NULL,
    attempt        INTEGER NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL,
    PRIMARY KEY (item_id, device_id, channel, attempt)
) STRICT;
CREATE TABLE findings (
    id             TEXT PRIMARY KEY,
    run_id         TEXT    NOT NULL REFERENCES runs (id),
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;
CREATE TABLE classifications (
    finding_id     TEXT    NOT NULL REFERENCES findings (id),
    version        INTEGER NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL,
    PRIMARY KEY (finding_id, version)
) STRICT;
CREATE TABLE resolved_policies (
    run_id         TEXT PRIMARY KEY REFERENCES runs (id),
    digest         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;
INSERT INTO resolved_policies VALUES('run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9',1,2,'{"run_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","digest":"sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","keys":[{"key":"budgets.stage_active_time","value":"1h","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"elaboration.max_iterations","value":"4","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"execution.capability_manifests","value":"[{\"encoding_version\":1,\"name\":\"Provider web read\",\"egress_profile\":\"provider_web_read\",\"digest\":\"sha256:214d34eb5a75854774206934802d17bc8dd5d258a5f1181073479db596cc95aa\"}]","provenance":{"source":"override","digest":"sha256:capability-policy"}},{"key":"gates.spec_approval","value":"true","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"paths","value":"daemon/**","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"research.allowlist","value":"example.com","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"research.max_response_bytes","value":"1024","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"waiting.spec_approval_attention_after","value":"1m","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}}]}');
INSERT INTO resolved_policies VALUES('implementation-from-submit','sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9',1,11,'{"run_id":"implementation-from-submit","digest":"sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","keys":[{"key":"budgets.stage_active_time","value":"1h","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"elaboration.max_iterations","value":"4","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"execution.capability_manifests","value":"[{\"encoding_version\":1,\"name\":\"Provider web read\",\"egress_profile\":\"provider_web_read\",\"digest\":\"sha256:214d34eb5a75854774206934802d17bc8dd5d258a5f1181073479db596cc95aa\"}]","provenance":{"source":"override","digest":"sha256:capability-policy"}},{"key":"gates.spec_approval","value":"true","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"paths","value":"daemon/**","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"research.allowlist","value":"example.com","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"research.max_response_bytes","value":"1024","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}},{"key":"waiting.spec_approval_attention_after","value":"1m","provenance":{"source":"override","digest":"sha256:abababababababababababababababababababababababababababababababab"}}]}');
CREATE TABLE outbox (
    id              INTEGER PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    kind            TEXT NOT NULL CHECK (kind <> ''),
    payload         BLOB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    created_at      TEXT NOT NULL
, payload_version INTEGER NOT NULL DEFAULT 1
    CHECK (payload_version IN (1, 2)), payload_digest TEXT NOT NULL DEFAULT '') STRICT;
INSERT INTO outbox VALUES(1,'inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1','elaboration_invocation_requested',X'7b2276657273696f6e223a2266726565736964652e656c61626f726174696f6e2d726571756573742f7631222c22656c61626f726174696f6e5f72756e5f6964223a2272756e2d656c61626f726174696f6e2d36393431393333333963353534356262656162656331396236666334363138323632356462373636393338313162613433356562313864636232363031313630222c22696d706c656d656e746174696f6e5f72756e5f6964223a22696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2270726f6a6563745f6964223a2270726f6a6563742d7375626d69742d656c61626f726174696f6e222c22696e766f636174696f6e5f6964223a22696e762d656c61626f726174652d72756e2d656c61626f726174696f6e2d363934313933333339633535343562626561626563313962366663343631383236323564623736363933383131626134333565623138646362323630313136302d31222c22697465726174696f6e223a312c22696e7075745f61727469666163745f696473223a5b2261727469666163742d73706563696669636174696f6e2d66366163396365353434646638376538303933393533303438356630326566373938366134643066303837656262646437643066663531653165626563343033225d2c22666565646261636b5f61727469666163745f696473223a6e756c6c2c22706f6c6963795f61727469666163745f6964223a2261727469666163742d706f6c6963792d35643939316439363035666631623765393766626263666465323339326230346161396633386536623433386666353539653262616130633465663966666439222c227075626c69636174696f6e223a7b227469746c65223a22546573742074686520776f726b206974656d222c22626f6479223a222323205768795c6e5c6e436c6f73657320233132332e5c6e222c22636f6d6d69745f617574686f72223a7b226170705f736c7567223a2266726565736964652d74657374222c22626f745f757365725f6964223a31323334357d7d2c227075626c69636174696f6e5f646967657374223a227368613235363a66336337393432323239353565333762636163643139396430646138376561346630636237323165646435613264353762363763653964383634303232313961222c2263616d706169676e5f6964223a2263616d706169676e2d62313363363438653537323564613231386232313338313836346535373637353066393364353431613235366261336562376135343466343738303039396261222c22617474656d70745f6e756d626572223a317d','dispatched','2026-09-02T04:05:16.928715Z',1,'sha256:7fd7eb3d3f8930e22cd632671957b949780800daa466b91a6883cfb646d22e2f');
INSERT INTO outbox VALUES(2,'claim-elaboration-implementation-implementation-from-submit','elaboration_implementation_claim',X'7b2276657273696f6e223a2266726565736964652e656c61626f726174696f6e2d726571756573742f7631222c22656c61626f726174696f6e5f72756e5f6964223a2272756e2d656c61626f726174696f6e2d36393431393333333963353534356262656162656331396236666334363138323632356462373636393338313162613433356562313864636232363031313630222c22696d706c656d656e746174696f6e5f72756e5f6964223a22696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2270726f6a6563745f6964223a2270726f6a6563742d7375626d69742d656c61626f726174696f6e222c22696e766f636174696f6e5f6964223a22696e762d656c61626f726174652d72756e2d656c61626f726174696f6e2d363934313933333339633535343562626561626563313962366663343631383236323564623736363933383131626134333565623138646362323630313136302d31222c22697465726174696f6e223a312c22696e7075745f61727469666163745f696473223a5b2261727469666163742d73706563696669636174696f6e2d66366163396365353434646638376538303933393533303438356630326566373938366134643066303837656262646437643066663531653165626563343033225d2c22666565646261636b5f61727469666163745f696473223a6e756c6c2c22706f6c6963795f61727469666163745f6964223a2261727469666163742d706f6c6963792d35643939316439363035666631623765393766626263666465323339326230346161396633386536623433386666353539653262616130633465663966666439222c227075626c69636174696f6e223a7b227469746c65223a22546573742074686520776f726b206974656d222c22626f6479223a222323205768795c6e5c6e436c6f73657320233132332e5c6e222c22636f6d6d69745f617574686f72223a7b226170705f736c7567223a2266726565736964652d74657374222c22626f745f757365725f6964223a31323334357d7d2c227075626c69636174696f6e5f646967657374223a227368613235363a66336337393432323239353565333762636163643139396430646138376561346630636237323165646435613264353762363763653964383634303232313961222c2263616d706169676e5f6964223a2263616d706169676e2d62313363363438653537323564613231386232313338313836346535373637353066393364353431613235366261336562376135343466343738303039396261222c22617474656d70745f6e756d626572223a317d','dispatched','2026-09-02T04:05:16.928769Z',1,'sha256:7fd7eb3d3f8930e22cd632671957b949780800daa466b91a6883cfb646d22e2f');
INSERT INTO outbox VALUES(3,'inv-explain-submitted-spec','agent_invocation_requested',X'7b22696e766f636174696f6e5f6964223a22696e762d6578706c61696e2d7375626d69747465642d73706563222c22636f6e766572736174696f6e5f6964223a22636f6e762d737065632d617070726f76616c2d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c226974656d5f6964223a22737065632d617070726f76616c2d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c226974656d5f76657273696f6e223a327d','dispatched','2026-09-02T04:05:17.053855Z',1,'sha256:8a88af760a1509aa3e0f82ba0d6b9ee6d0316e57061c1eae023f17b9586facfe');
INSERT INTO outbox VALUES(4,'elaboration-discussion-explain-submitted-spec','elaboration_discussion_requested',X'7b2276657273696f6e223a2266726565736964652e656c61626f726174696f6e2d64697363757373696f6e2d726571756573742f7631222c22656c61626f726174696f6e5f72756e5f6964223a2272756e2d656c61626f726174696f6e2d36393431393333333963353534356262656162656331396236666334363138323632356462373636393338313162613433356562313864636232363031313630222c22696d706c656d656e746174696f6e5f72756e5f6964223a22696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2270726f6a6563745f6964223a2270726f6a6563742d7375626d69742d656c61626f726174696f6e222c22697465726174696f6e223a312c22696e766f636174696f6e5f6964223a22656c61626f726174696f6e2d64697363757373696f6e2d6578706c61696e2d7375626d69747465642d73706563222c22646973637573735f696e766f636174696f6e5f6964223a22696e762d6578706c61696e2d7375626d69747465642d73706563222c22636f6e766572736174696f6e5f6964223a22636f6e762d737065632d617070726f76616c2d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c227468726f7567685f73657175656e6365223a312c227072656669785f646967657374223a227368613235363a39373430653739393339333065386266353934646565383662626437356237626235376233393562313233366365336562313639373065643461366437346338222c226974656d5f6964223a22737065632d617070726f76616c2d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c226974656d5f76657273696f6e223a322c22696e7075745f61727469666163745f696473223a5b2261727469666163742d73706563696669636174696f6e2d66366163396365353434646638376538303933393533303438356630326566373938366134643066303837656262646437643066663531653165626563343033222c22737065632d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c22737065632d64697363757373696f6e2d6578706c61696e2d7375626d69747465642d73706563225d2c22737065635f61727469666163745f6964223a22737065632d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c22706f6c6963795f61727469666163745f6964223a2261727469666163742d706f6c6963792d35643939316439363035666631623765393766626263666465323339326230346161396633386536623433386666353539653262616130633465663966666439227d','dispatched','2026-09-02T04:05:17.073832Z',1,'sha256:1d5564eee1013e3aeda16951abd9e855ee6a8f62a9b46037a04527e78f8b76b6');
INSERT INTO outbox VALUES(5,'inv-implement-implementation-from-submit','production_invocation_requested',X'7b2276657273696f6e223a2266726565736964652e70726f64756374696f6e2d696e766f636174696f6e2f7632222c22696e766f636174696f6e5f6964223a22696e762d696d706c656d656e742d696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2272756e5f6964223a22696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2273746167655f6964223a22696d706c656d656e742d696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c227075626c69636174696f6e223a7b227469746c65223a22546573742074686520776f726b206974656d222c22626f6479223a222323205768795c6e5c6e436c6f73657320233132332e5c6e222c22636f6d6d69745f617574686f72223a7b226170705f736c7567223a2266726565736964652d74657374222c22626f745f757365725f6964223a31323334357d7d7d','pending','2026-09-02T04:05:17.078299Z',1,'sha256:95ffd18543d936497890632da872db2d733035a5dc10036d9f42bc5ee09f5d2e');
INSERT INTO outbox VALUES(6,'publish/publish-production-implementation-from-submit/publish.publication','publish.invocation_reservation',X'7b2276657273696f6e223a2266726565736964652d7075626c69636174696f6e2d7265736572766174696f6e2f7631222c22696e766f636174696f6e5f6964223a227075626c6973682d70726f64756374696f6e2d696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2272756e5f6964223a22696d706c656d656e746174696f6e2d66726f6d2d7375626d6974227d','pending','2026-09-02T04:05:17.078408Z',1,'sha256:81c389f9b118ac5d2ce09e973a9f1298913034e16677e4679e6bf2a510fafe3c');
CREATE TABLE inbox (
    id              INTEGER PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    kind            TEXT NOT NULL CHECK (kind <> ''),
    payload         BLOB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    created_at      TEXT NOT NULL
) STRICT;
INSERT INTO inbox VALUES(1,'inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1','elaboration_stage_terminal',X'7b22696e766f636174696f6e5f6964223a22696e762d656c61626f726174652d72756e2d656c61626f726174696f6e2d363934313933333339633535343562626561626563313962366663343631383236323564623736363933383131626134333565623138646362323630313136302d31222c22697465726174696f6e223a312c22737461747573223a22636f6d706c65746564222c2272657365617263685f61727469666163745f696473223a5b5d2c22737065635f61727469666163745f6964223a22737065632d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c22617070726f76616c5f6974656d5f6964223a22737065632d617070726f76616c2d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c2273756d6d6172795f646967657374223a227368613235363a38653731346635336131393734643562616666616239396166663765383366636438663936643566666333366331336237336665613062656439623136646665227d','pending','2026-09-02T04:05:17.033724Z');
INSERT INTO inbox VALUES(2,'inv-explain-submitted-spec','agent_completion',X'7b22696e766f636174696f6e5f6964223a22696e762d6578706c61696e2d7375626d69747465642d73706563222c22626f6479223a224974206b656570732074686520776f726b666c6f7720626f756e6465642062792070696e6e696e672074686520617070726f76656420617274696661637420616e64206465636c617265642073636f70652e222c226174746163686d656e7473223a6e756c6c7d','pending','2026-09-02T04:05:17.171037Z');
INSERT INTO inbox VALUES(3,'elaboration-discussion-explain-submitted-spec','elaboration_discussion_terminal',X'7b22696e766f636174696f6e5f6964223a22656c61626f726174696f6e2d64697363757373696f6e2d6578706c61696e2d7375626d69747465642d73706563222c22646973637573735f696e766f636174696f6e5f6964223a22696e762d6578706c61696e2d7375626d69747465642d73706563222c227265706c79223a224974206b656570732074686520776f726b666c6f7720626f756e6465642062792070696e6e696e672074686520617070726f76656420617274696661637420616e64206465636c617265642073636f70652e222c2264656c697665726564223a747275657d','pending','2026-09-02T04:05:17.173046Z');
CREATE TABLE commands (
    command_id     TEXT PRIMARY KEY,
    item_id        TEXT    NOT NULL REFERENCES attention_items (id),
    item_version   INTEGER NOT NULL,
    pr_head_sha    TEXT    NOT NULL,
    device_id      TEXT    NOT NULL,
    action         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
, backup_binding_digest TEXT NOT NULL DEFAULT '') STRICT;
INSERT INTO commands VALUES('explain-submitted-spec','spec-approval-implementation-from-submit-1',1,'','device-submit','discuss',1,8,'{"command":{"command_id":"explain-submitted-spec","device_id":"device-submit","item_id":"spec-approval-implementation-from-submit-1","item_version":1,"pr_head_sha":"","artifact_digests":["sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","sha256:8e714f53a1974d5baffab99aff7e83fcd8f96d5ffc36c13b73fea0bed9b16dfe"],"action":"discuss","message":"Why does the specification keep the workflow bounded?","attachments":[]},"inline_claims":[{"digest":"sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","content":"# Approved specification\n\nImplement the thing within the declared paths."},{"digest":"sha256:8e714f53a1974d5baffab99aff7e83fcd8f96d5ffc36c13b73fea0bed9b16dfe","content":"Ready for implementation."}]}','sha256:4592fc23c8649a5bfa50abb6722f267d38b21b2c4fd9045f47e1d0a30590095e');
INSERT INTO commands VALUES('approve-submitted-spec','spec-approval-implementation-from-submit-1',2,'','device-submit','approve',1,9,'{"command":{"command_id":"approve-submitted-spec","device_id":"device-submit","item_id":"spec-approval-implementation-from-submit-1","item_version":2,"pr_head_sha":"","artifact_digests":["sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","sha256:8e714f53a1974d5baffab99aff7e83fcd8f96d5ffc36c13b73fea0bed9b16dfe"],"action":"approve","message":"","attachments":[]},"inline_claims":[{"digest":"sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","content":"# Approved specification\n\nImplement the thing within the declared paths."},{"digest":"sha256:8e714f53a1974d5baffab99aff7e83fcd8f96d5ffc36c13b73fea0bed9b16dfe","content":"Ready for implementation."}]}','sha256:04de9154c0c123d6ca4f604f850e797476182f5e606be3b473976553194882b7');
CREATE TABLE devices (
    id             TEXT PRIMARY KEY,
    status         TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;
INSERT INTO devices VALUES('device-submit','active',1,3,'{"id":"device-submit","display_name":"Operator","status":"active","paired_at":"2026-09-02T04:05:16Z","revoked_at":null}');
CREATE TABLE device_credentials (
    device_id       TEXT PRIMARY KEY REFERENCES devices (id),
    credential_kind TEXT NOT NULL,
    credential      TEXT NOT NULL
) STRICT;
CREATE TABLE pairing_codes (
    code_hash   TEXT PRIMARY KEY,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    consumed_at TEXT,
    device_id   TEXT UNIQUE REFERENCES devices (id),
    body        TEXT NOT NULL,
    CHECK ((consumed_at IS NULL) = (device_id IS NULL))
) STRICT;
CREATE TABLE publish_mint_audits (
    id                      INTEGER PRIMARY KEY,
    minted_at               TEXT    NOT NULL CHECK (minted_at <> ''),
    installation_id         INTEGER NOT NULL CHECK (installation_id > 0),
    repo                    TEXT    NOT NULL CHECK (repo <> ''),
    requested_contents      TEXT    NOT NULL,
    requested_pull_requests TEXT    NOT NULL,
    requested_metadata      TEXT    NOT NULL,
    granted_contents        TEXT    NOT NULL,
    granted_pull_requests   TEXT    NOT NULL,
    granted_metadata        TEXT    NOT NULL,
    expires_at              TEXT    NOT NULL CHECK (expires_at <> '')
, requested_actions TEXT NOT NULL DEFAULT '', requested_administration TEXT NOT NULL DEFAULT '', requested_environments TEXT NOT NULL DEFAULT '', granted_actions TEXT NOT NULL DEFAULT '', granted_administration TEXT NOT NULL DEFAULT '', granted_environments TEXT NOT NULL DEFAULT '', registration_id INTEGER NOT NULL DEFAULT 0
    CHECK (registration_id >= 0), repository_id INTEGER NOT NULL DEFAULT 0
    CHECK (repository_id >= 0)) STRICT;
CREATE TABLE trust_profiles (
    profile_digest TEXT NOT NULL PRIMARY KEY CHECK (profile_digest <> ''),
    repo           TEXT NOT NULL CHECK (repo <> ''),
    recorded_at    TEXT NOT NULL CHECK (recorded_at <> ''),
    body           TEXT NOT NULL,
    UNIQUE (repo, profile_digest)
) STRICT;
CREATE TABLE workflow_audits (
    id                    INTEGER PRIMARY KEY,
    repo                  TEXT NOT NULL CHECK (repo <> ''),
    audited_commit_sha    TEXT NOT NULL CHECK (audited_commit_sha <> ''),
    audited_at            TEXT NOT NULL CHECK (audited_at <> ''),
    workflow_audit_digest TEXT NOT NULL CHECK (workflow_audit_digest <> ''),
    body                  TEXT NOT NULL
) STRICT;
CREATE TABLE candidate_authorizations (
    id                   TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    repo                 TEXT NOT NULL CHECK (repo <> ''),
    base_sha             TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha             TEXT NOT NULL CHECK (head_sha <> ''),
    trust_profile_digest TEXT NOT NULL CHECK (trust_profile_digest <> ''),
    created_at           TEXT NOT NULL CHECK (created_at <> ''),
    body                 TEXT NOT NULL,
    UNIQUE (repo, head_sha, trust_profile_digest),
    FOREIGN KEY (repo, trust_profile_digest) REFERENCES trust_profiles(repo, profile_digest)
) STRICT;
CREATE TABLE legacy_candidate_authorizations (
    id                   TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    repo                 TEXT NOT NULL CHECK (repo <> ''),
    base_sha             TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha             TEXT NOT NULL CHECK (head_sha <> ''),
    trust_profile_digest TEXT NOT NULL CHECK (trust_profile_digest <> ''),
    created_at           TEXT NOT NULL CHECK (created_at <> ''),
    body                 TEXT NOT NULL,
    retired_reason       TEXT NOT NULL CHECK (retired_reason <> ''),
    FOREIGN KEY (repo, trust_profile_digest) REFERENCES trust_profiles(repo, profile_digest)
) STRICT;
CREATE TABLE auth_identities (
    id                        TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    provider                  TEXT NOT NULL CHECK (provider <> ''),
    auth_store_mutation_lease INTEGER NOT NULL CHECK (auth_store_mutation_lease IN (0, 1)),
    max_parallel_executions   INTEGER NOT NULL CHECK (max_parallel_executions >= 1),
    refresh_strategy          TEXT    NOT NULL CHECK (refresh_strategy <> ''),
    supports_read_only_auth_snapshot INTEGER NOT NULL
        CHECK (supports_read_only_auth_snapshot IN (0, 1)),
    recorded_at               TEXT NOT NULL CHECK (recorded_at <> ''),
    body                      TEXT NOT NULL
, auth_store_volume TEXT, account_binding TEXT NOT NULL DEFAULT '', usage_pool TEXT NOT NULL DEFAULT '', budget INTEGER NOT NULL DEFAULT 0 CHECK (budget >= 0), enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)), cost_owner TEXT NOT NULL DEFAULT '') STRICT;
INSERT INTO auth_identities VALUES('auth-submit','codex',1,2,'refresh_on_demand',0,'2026-09-02T04:05:16Z','{"identity":{"id":"auth-submit","provider":"codex","account_binding":"","usage_pool":"","budget":0,"auth_store_mutation_lease":true,"max_parallel_executions":2,"enabled":false,"cost_owner":"","interim":{"auth_store_volume":"provider-credentials","refresh_strategy":"refresh_on_demand","supports_read_only_auth_snapshot":false}},"recorded_at":"2026-09-02T04:05:16Z"}','provider-credentials','','',0,0,'');
CREATE TABLE auth_store_mutation_leases (
    auth_identity_id     TEXT    NOT NULL PRIMARY KEY REFERENCES auth_identities (id),
    holder               TEXT    NOT NULL CHECK (holder <> ''),
    fence                INTEGER NOT NULL CHECK (fence > 0),
    acquired_at          TEXT    NOT NULL CHECK (acquired_at <> ''),
    expires_at           TEXT    NOT NULL CHECK (expires_at <> ''),
    expires_at_unix_nano INTEGER NOT NULL,
    released_at          TEXT,
    body                 TEXT    NOT NULL
) STRICT;
CREATE TABLE execution_admissions (
    invocation_id    TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    id               TEXT NOT NULL UNIQUE CHECK (id <> ''),
    run_id           TEXT NOT NULL REFERENCES runs (id),
    stage_id         TEXT NOT NULL CHECK (stage_id <> ''),
    attempt_id       TEXT NOT NULL CHECK (attempt_id <> ''),
    operating_mode   TEXT NOT NULL CHECK (operating_mode <> ''),
    auth_identity_id TEXT REFERENCES auth_identities (id),
    admitted_at      TEXT NOT NULL CHECK (admitted_at <> ''),
    body             TEXT NOT NULL
, agent_digest TEXT, enrollment_id TEXT REFERENCES client_enrollments (id), enrollment_generation INTEGER) STRICT;
INSERT INTO execution_admissions VALUES('inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1','sha256:894c50d8be483cdb8a8c0e2813e247b0ba2530d6fa1a089042b4f193f49c06bf','run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','attempt-inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1','attended_dev','auth-submit','2026-09-02T04:05:16Z','{"id":"sha256:894c50d8be483cdb8a8c0e2813e247b0ba2530d6fa1a089042b4f193f49c06bf","invocation_id":"inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","run_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","stage_id":"elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","attempt_id":"attempt-inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","backend":"submit-elaboration-test","capabilities":["supports_post_exit_export"],"operating_mode":"attended_dev","credential_mode":"subscription_contained","egress_profile":"provider_only","image_ref":"agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","spec_digest":"sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","policy_digest":"sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","input_digest":"sha256:95233f10c598d65f634f2bc7f2d091f55002290941b5e797bb06a6c39ededaef","stage_inputs":{"id":"sha256:ec3fd1bb64bb6d9e364c51be7894b340254eb4974d0f2da6332010fe74286e24","input_digest":"sha256:95233f10c598d65f634f2bc7f2d091f55002290941b5e797bb06a6c39ededaef","specification_digest":"sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","prompt_package_digest":"sha256:c1ee3291d49de2c0d87cd8708fec0cf11c7c5976f8e90924a10b354760f3ccad","policy_digest":"sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","vendor_instructions":{"vendor":"codex","delivery":"append_file","digest":"sha256:4fb0d11956ae46715bc84ea8561ff63c9e6d6ce30c54062717a4be1601221615"},"prior_artifact_digests":[],"image_input_digests":[]},"base":{"repo":"owner/repo","repository_id":1,"base_ref":"refs/heads/main","base_sha":"deadbeef"},"workspace":"workspace-submit","auth_identity_id":"auth-submit","trust_profile_digest":null,"backup_encryption_waiver":null,"admitted_at":"2026-09-02T04:05:16Z"}',NULL,NULL,NULL);
INSERT INTO execution_admissions VALUES('elaboration-discussion-explain-submitted-spec','sha256:fd887c7674097a5271c9da23f8fe3f3dc0ca1e71c0da53026bcaa6d185bb959d','run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','attempt-elaboration-discussion-explain-submitted-spec','attended_dev','auth-submit','2026-09-02T04:05:16Z','{"id":"sha256:fd887c7674097a5271c9da23f8fe3f3dc0ca1e71c0da53026bcaa6d185bb959d","invocation_id":"elaboration-discussion-explain-submitted-spec","run_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","stage_id":"elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","attempt_id":"attempt-elaboration-discussion-explain-submitted-spec","backend":"submit-elaboration-test","capabilities":["supports_post_exit_export"],"operating_mode":"attended_dev","credential_mode":"subscription_contained","egress_profile":"provider_only","image_ref":"agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","spec_digest":"sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","policy_digest":"sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","input_digest":"sha256:6a2b30291fd8a81ba00a88f8ccf2aab60ea87b21e1e1cae667261d1e926c2816","stage_inputs":{"id":"sha256:139f5fceb628a0aad21eb3af5940c96f4d2559ffe7153d3bbf8fe13e7dd86a04","input_digest":"sha256:6a2b30291fd8a81ba00a88f8ccf2aab60ea87b21e1e1cae667261d1e926c2816","specification_digest":"sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","prompt_package_digest":"sha256:c1ee3291d49de2c0d87cd8708fec0cf11c7c5976f8e90924a10b354760f3ccad","policy_digest":"sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","vendor_instructions":{"vendor":"codex","delivery":"append_file","digest":"sha256:4fb0d11956ae46715bc84ea8561ff63c9e6d6ce30c54062717a4be1601221615"},"conversation_digest":"sha256:9740e7993930e8bf594dee86bbd75b7bb57b395b1236ce3eb16970ed4a6d74c8","prior_artifact_digests":["sha256:abb50323f7583f36ca81f61f4f29704a77bfa1004079256304d23d9b2c2ec542","sha256:b4389ecc569a8107d88acc8097b270535305ab4a69907a5cfb8e7ead211a3bd4"],"image_input_digests":[]},"base":{"repo":"owner/repo","repository_id":1,"base_ref":"refs/heads/main","base_sha":"deadbeef"},"workspace":"workspace-submit","auth_identity_id":"auth-submit","trust_profile_digest":null,"backup_encryption_waiver":null,"admitted_at":"2026-09-02T04:05:16Z"}',NULL,NULL,NULL);
CREATE TABLE execution_exports (
    invocation_id            TEXT NOT NULL PRIMARY KEY
                                 REFERENCES execution_admissions (invocation_id),
    admission_id             TEXT NOT NULL REFERENCES execution_admissions (id),
    observed_base_sha        TEXT NOT NULL CHECK (observed_base_sha <> ''),
    head_sha                 TEXT NOT NULL CHECK (head_sha <> ''),
    manifest_digest          TEXT NOT NULL CHECK (manifest_digest <> ''),
    evidence_manifest_digest TEXT,
    commit_plan_present      INTEGER NOT NULL CHECK (commit_plan_present IN (0, 1)),
    recorded_at              TEXT NOT NULL CHECK (recorded_at <> ''),
    body                     TEXT NOT NULL
) STRICT;
CREATE TABLE local_backup_checkpoint_marker (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    generated_at TEXT NOT NULL CHECK (generated_at <> '')
) STRICT;
CREATE TABLE local_backup_restore_marker (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    checkpoint_digest TEXT NOT NULL CHECK (checkpoint_digest <> ''),
    restored_at       TEXT NOT NULL CHECK (restored_at <> '')
) STRICT;
CREATE TABLE project_images (
    id             TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    repository     TEXT NOT NULL CHECK (repository <> ''),
    repository_id  INTEGER NOT NULL CHECK (repository_id > 0),
    commit_sha     TEXT NOT NULL CHECK (length(commit_sha) = 40),
    recipe_digest  TEXT NOT NULL CHECK (recipe_digest <> ''),
    base_image_ref TEXT NOT NULL CHECK (base_image_ref <> ''),
    image_ref      TEXT NOT NULL UNIQUE CHECK (image_ref <> ''),
    body           TEXT NOT NULL
) STRICT;
CREATE TABLE unattended_operation_transitions (
    id          INTEGER PRIMARY KEY,
    state       TEXT NOT NULL CHECK (state IN ('stopped', 'resumed')),
    command_id  TEXT REFERENCES commands (command_id) CHECK (command_id IS NULL OR command_id <> ''),
    reason      TEXT NOT NULL,
    occurred_at TEXT NOT NULL CHECK (occurred_at <> '')
) STRICT;
CREATE TABLE backend_conformance_records (
    id           INTEGER PRIMARY KEY,
    backend      TEXT NOT NULL CHECK (backend <> ''),
    outcome      TEXT NOT NULL CHECK (outcome <> ''),
    -- Canonical CapabilitySnapshot JSON; the literal 'null' exactly when the
    -- pass failed (a failed pass proves nothing).
    capabilities TEXT NOT NULL CHECK (capabilities <> ''),
    proved_at    TEXT NOT NULL CHECK (proved_at <> '')
, configuration_digest TEXT NOT NULL
    DEFAULT 'sha256:0000000000000000000000000000000000000000000000000000000000000000'
    CHECK (configuration_digest <> '')) STRICT;
CREATE TABLE execution_outcomes (
    invocation_id TEXT NOT NULL PRIMARY KEY
                       REFERENCES execution_admissions (invocation_id),
    admission_id  TEXT NOT NULL REFERENCES execution_admissions (id),
    status        TEXT NOT NULL CHECK (status <> ''),
    summary       TEXT NOT NULL,
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
) STRICT;
CREATE TABLE handoff_journal_records (
    run_id                    TEXT NOT NULL PRIMARY KEY CHECK (run_id <> ''),
    ownership_token           TEXT NOT NULL CHECK (ownership_token <> ''),
    spec_digest               TEXT NOT NULL CHECK (spec_digest <> ''),
    observed_base_sha         TEXT NOT NULL,
    credential_pre_digest     TEXT NOT NULL,
    writer_complete           INTEGER NOT NULL CHECK (writer_complete IN (0, 1)),
    cancellation_requested    INTEGER NOT NULL CHECK (cancellation_requested IN (0, 1)),
    writer_failure_status     INTEGER CHECK (writer_failure_status BETWEEN 1 AND 255),
    state_preparation         TEXT NOT NULL,
    instruction_preparation   TEXT NOT NULL,
    lease_auth_identity_id    TEXT REFERENCES auth_identities (id),
    lease_holder              TEXT,
    lease_fence               INTEGER,
    lease_acquired_at         TEXT,
    lease_expires_at          TEXT,
    export_dir                TEXT NOT NULL,
    outcome                   TEXT CHECK (outcome IS NULL OR outcome IN ('completed', 'canceled', 'failed', 'loss')),
    opened_at                 TEXT NOT NULL CHECK (opened_at <> ''),
    body                      TEXT NOT NULL,
    CHECK (
        (lease_auth_identity_id IS NULL AND lease_holder IS NULL AND
         lease_fence IS NULL AND lease_acquired_at IS NULL AND lease_expires_at IS NULL)
        OR
        (lease_auth_identity_id IS NOT NULL AND lease_holder IS NOT NULL AND
         lease_fence > 0 AND lease_acquired_at IS NOT NULL AND lease_expires_at IS NOT NULL)
    )
) STRICT;
CREATE TABLE workflow_audit_evidence (
    repo                  TEXT NOT NULL CHECK (repo <> ''),
    workflow_audit_digest TEXT NOT NULL CHECK (workflow_audit_digest <> ''),
    body                  BLOB NOT NULL
                          CHECK (length(body) > 0 AND length(body) <= 16777216),
    PRIMARY KEY (repo, workflow_audit_digest)
) STRICT;
CREATE TABLE trust_profile_activations (
    id                    INTEGER PRIMARY KEY,
    repo                  TEXT NOT NULL CHECK (repo <> ''),
    profile_digest        TEXT NOT NULL CHECK (profile_digest <> ''),
    workflow_audit_digest TEXT NOT NULL CHECK (workflow_audit_digest <> ''),
    activated_at          TEXT NOT NULL CHECK (activated_at <> ''),
    FOREIGN KEY (repo, profile_digest) REFERENCES trust_profiles(repo, profile_digest)
) STRICT;
CREATE TABLE run_milestones (
    id            INTEGER PRIMARY KEY,
    run_id        TEXT NOT NULL CHECK (run_id <> ''),
    kind          TEXT NOT NULL CHECK (kind IN (
                      'run_submitted', 'invocation_admitted',
                      'invocation_started', 'execution_export_recorded',
                      'execution_outcome_recorded', 'terminal_recorded',
                      'publication_ready', 'publication_blocked')),
    invocation_id TEXT CHECK (invocation_id IS NULL OR invocation_id <> ''),
    terminal      TEXT CHECK (terminal IS NULL OR terminal <> ''),
    outcome       TEXT CHECK (outcome IS NULL OR outcome <> ''),
    reason        TEXT CHECK (reason IS NULL OR reason <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;
INSERT INTO run_milestones VALUES(1,'run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','run_submitted','inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1',NULL,NULL,NULL,'2026-09-02T04:05:16.928823Z');
INSERT INTO run_milestones VALUES(2,'run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','invocation_admitted','inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1',NULL,NULL,NULL,'2026-09-02T04:05:16Z');
INSERT INTO run_milestones VALUES(3,'run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','invocation_started','inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1',NULL,NULL,NULL,'2026-09-02T04:05:17.003389Z');
INSERT INTO run_milestones VALUES(4,'implementation-from-submit','run_submitted','inv-implement-implementation-from-submit',NULL,NULL,NULL,'2026-09-02T04:05:17.078476Z');
INSERT INTO run_milestones VALUES(5,'run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','invocation_admitted','elaboration-discussion-explain-submitted-spec',NULL,NULL,NULL,'2026-09-02T04:05:16Z');
INSERT INTO run_milestones VALUES(6,'run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','invocation_started','elaboration-discussion-explain-submitted-spec',NULL,NULL,NULL,'2026-09-02T04:05:17.140751Z');
CREATE TABLE invocation_observations (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL CHECK (run_id <> ''),
    status        TEXT NOT NULL CHECK (status <> ''),
    live          INTEGER NOT NULL CHECK (live IN (0, 1)),
    observed_at   TEXT NOT NULL CHECK (observed_at <> '')
) STRICT;
INSERT INTO invocation_observations VALUES('inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1','run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','completed',0,'2026-09-02T04:05:17.020596Z');
INSERT INTO invocation_observations VALUES('elaboration-discussion-explain-submitted-spec','run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','completed',0,'2026-09-02T04:05:17.170019Z');
CREATE TABLE run_hold_observations (
    run_id            TEXT NOT NULL PRIMARY KEY CHECK (run_id <> ''),
    invocation_id     TEXT CHECK (invocation_id IS NULL OR invocation_id <> ''),
    reason            TEXT NOT NULL CHECK (reason <> ''),
    first_observed_at TEXT NOT NULL CHECK (first_observed_at <> ''),
    last_observed_at  TEXT NOT NULL CHECK (last_observed_at <> '')
) STRICT;
INSERT INTO run_hold_observations VALUES('implementation-from-submit','inv-implement-implementation-from-submit','attended_mode_active','2026-09-02T04:05:17.141511Z','2026-09-02T04:05:17.141511Z');
CREATE TABLE schedule_timers (
    schedule_id          TEXT NOT NULL PRIMARY KEY CHECK (schedule_id <> ''),
    generation           INTEGER NOT NULL CHECK (generation >= 1),
    next_nominal_fire_at INTEGER NOT NULL
) STRICT;
CREATE TABLE schedule_occurrences (
    id              INTEGER PRIMARY KEY,
    schedule_id     TEXT NOT NULL CHECK (schedule_id <> ''),
    generation      INTEGER NOT NULL CHECK (generation >= 1),
    nominal_fire_at INTEGER NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'consumed')),
    gap_missed      INTEGER CHECK (gap_missed IS NULL OR gap_missed >= 1),
    gap_earliest    INTEGER,
    created_at      TEXT NOT NULL CHECK (created_at <> ''),
    consumed_at     TEXT CHECK (consumed_at IS NULL OR consumed_at <> ''),
    outcome         TEXT CHECK (outcome IS NULL OR outcome <> ''),
    CHECK ((gap_missed IS NULL) = (gap_earliest IS NULL)),
    CHECK ((status = 'consumed') = (consumed_at IS NOT NULL)),
    CHECK ((status = 'consumed') = (outcome IS NOT NULL))
) STRICT;
CREATE TABLE work_unit_declarations (
    unit_id     TEXT NOT NULL PRIMARY KEY CHECK (unit_id <> ''),
    run_id      TEXT NOT NULL UNIQUE REFERENCES runs (id),
    project_id  TEXT NOT NULL CHECK (project_id <> ''),
    body        TEXT NOT NULL CHECK (body <> ''),
    declared_at TEXT NOT NULL CHECK (declared_at <> '')
) STRICT;
CREATE TABLE work_unit_pr_bindings (
    unit_id       TEXT NOT NULL PRIMARY KEY
                      REFERENCES work_unit_declarations (unit_id),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number     INTEGER NOT NULL CHECK (pr_number > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;
CREATE TABLE pull_merge_facts (
    id            INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number     INTEGER NOT NULL CHECK (pr_number > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> '')
) STRICT;
CREATE TABLE issue_state_facts (
    id            INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    issue_number  INTEGER NOT NULL CHECK (issue_number > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> '')
) STRICT;
CREATE TABLE work_unit_completions (
    unit_id     TEXT NOT NULL PRIMARY KEY
                    REFERENCES work_unit_declarations (unit_id),
    body        TEXT NOT NULL CHECK (body <> ''),
    recorded_at TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;
CREATE TABLE schedules (
    id             TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    project_id     TEXT NOT NULL CHECK (project_id <> ''),
    kind           TEXT NOT NULL CHECK (kind IN (
                       'pr_checks_deadline', 'review_wait_threshold',
                       'base_advance_watch', 'installation_poll',
                       'doctor', 'janitor')),
    status         TEXT NOT NULL CHECK (status IN (
                       'armed', 'fired', 'resolved', 'expired')),
    generation     INTEGER NOT NULL CHECK (generation >= 1),
    run_id         TEXT REFERENCES runs(id),
    policy_digest  TEXT CHECK (policy_digest IS NULL OR policy_digest <> ''),
    fire_at        INTEGER,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT NOT NULL,
    CHECK (
        (kind IN ('pr_checks_deadline', 'review_wait_threshold', 'base_advance_watch'))
        = (run_id IS NOT NULL AND policy_digest IS NOT NULL)
    )
) STRICT;
CREATE TABLE ready_item_pr_bindings (
    item_id       TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    run_id        TEXT NOT NULL UNIQUE REFERENCES runs (id),
    producing_invocation_id TEXT NOT NULL REFERENCES execution_admissions (invocation_id),
    publication_invocation_id TEXT NOT NULL,
    publication_identity TEXT NOT NULL CHECK (publication_identity <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number     INTEGER NOT NULL CHECK (pr_number > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;
CREATE TABLE review_records (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    round         INTEGER NOT NULL CHECK (round > 0),
    base_sha      TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha      TEXT NOT NULL CHECK (head_sha <> ''),
    outcome       TEXT NOT NULL CHECK (outcome IN ('clean', 'findings')),
    completed_at  TEXT NOT NULL CHECK (completed_at <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> ''),
    UNIQUE (run_id, round)
) STRICT;
CREATE TABLE review_record_findings (
    invocation_id TEXT NOT NULL REFERENCES review_records (invocation_id),
    finding_id    TEXT NOT NULL REFERENCES findings (id),
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    PRIMARY KEY (invocation_id, finding_id),
    UNIQUE (invocation_id, ordinal)
) STRICT;
CREATE TABLE review_failures (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    round         INTEGER NOT NULL CHECK (round > 0),
    failure_class TEXT NOT NULL CHECK (failure_class IN
        ('transient', 'configuration', 'quota', 'contradiction')),
    observed_at   TEXT NOT NULL CHECK (observed_at <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> ''),
    UNIQUE (run_id, round)
) STRICT;
CREATE TABLE codex_review_workspaces (
    source_run_id TEXT NOT NULL PRIMARY KEY CHECK (source_run_id <> ''),
    volume        TEXT NOT NULL UNIQUE CHECK (volume <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE codex_review_intents (
    run_id      TEXT NOT NULL PRIMARY KEY CHECK (run_id <> ''),
    state       TEXT NOT NULL CHECK (state IN
        ('preparing', 'prepared', 'starting', 'started', 'closed')),
    body_digest TEXT NOT NULL CHECK (body_digest <> ''),
    body        TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE codex_review_bindings (
    run_id      TEXT NOT NULL PRIMARY KEY REFERENCES codex_review_intents (run_id),
    body_digest TEXT NOT NULL CHECK (body_digest <> ''),
    body        TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE codex_review_requests (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE codex_review_outcomes (
    invocation_id TEXT NOT NULL PRIMARY KEY REFERENCES codex_review_requests (invocation_id),
    state         TEXT NOT NULL CHECK (state IN ('collected', 'ready')),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE native_review_observations (
    id            INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number     INTEGER NOT NULL CHECK (pr_number > 0),
    provider      TEXT NOT NULL CHECK (provider <> ''),
    kind          TEXT NOT NULL CHECK (kind <> ''),
    native_id     INTEGER NOT NULL CHECK (native_id > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> '')
) STRICT;
CREATE TABLE review_retries (
    run_id        TEXT NOT NULL PRIMARY KEY REFERENCES runs (id),
    invocation_id TEXT NOT NULL CHECK (invocation_id <> ''),
    round         INTEGER NOT NULL CHECK (round > 0),
    base_sha      TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha      TEXT NOT NULL CHECK (head_sha <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE review_recovery_transitions (
    id             INTEGER PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES runs (id),
    invocation_id  TEXT NOT NULL REFERENCES review_failures (invocation_id),
    round           INTEGER NOT NULL CHECK (round > 0),
    base_sha        TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha        TEXT NOT NULL CHECK (head_sha <> ''),
    failure_digest  TEXT NOT NULL CHECK (failure_digest <> ''),
    command_id      TEXT REFERENCES commands (command_id)
                         CHECK (command_id IS NULL OR command_id <> ''),
    reason          TEXT NOT NULL CHECK (reason <> ''),
    occurred_at     TEXT NOT NULL CHECK (occurred_at <> ''),
    UNIQUE (run_id, invocation_id, failure_digest)
) STRICT;
CREATE TABLE publish_installation_mint_audits (
    id                       INTEGER PRIMARY KEY,
    minted_at                TEXT    NOT NULL CHECK (minted_at <> ''),
    registration_id          INTEGER NOT NULL CHECK (registration_id > 0),
    installation_id          INTEGER NOT NULL CHECK (installation_id > 0),
    outcome                  TEXT    NOT NULL CHECK (outcome <> ''),
    requested_actions        TEXT    NOT NULL,
    requested_administration TEXT    NOT NULL,
    requested_contents       TEXT    NOT NULL,
    requested_environments   TEXT    NOT NULL,
    requested_pull_requests  TEXT    NOT NULL,
    requested_metadata       TEXT    NOT NULL,
    granted_actions          TEXT    NOT NULL,
    granted_administration   TEXT    NOT NULL,
    granted_contents         TEXT    NOT NULL,
    granted_environments     TEXT    NOT NULL,
    granted_pull_requests    TEXT    NOT NULL,
    granted_metadata         TEXT    NOT NULL,
    expires_at               TEXT    CHECK (expires_at IS NULL OR expires_at <> '')
) STRICT;
CREATE TABLE review_configuration_recovery_transitions (
    id                         INTEGER PRIMARY KEY,
    run_id                     TEXT NOT NULL REFERENCES runs (id),
    invocation_id              TEXT NOT NULL REFERENCES review_failures (invocation_id),
    round                      INTEGER NOT NULL CHECK (round > 0),
    base_sha                   TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha                   TEXT NOT NULL CHECK (head_sha <> ''),
    failure_digest             TEXT NOT NULL CHECK (failure_digest <> ''),
    repo                       TEXT NOT NULL CHECK (repo <> ''),
    repository_id              INTEGER NOT NULL CHECK (repository_id > 0),
    superseded_profile_digest  TEXT NOT NULL REFERENCES trust_profiles (profile_digest),
    superseding_profile_digest TEXT NOT NULL REFERENCES trust_profiles (profile_digest),
    command_id                 TEXT REFERENCES commands (command_id)
                                    CHECK (command_id IS NULL OR command_id <> ''),
    reason                     TEXT NOT NULL CHECK (reason <> ''),
    occurred_at                TEXT NOT NULL CHECK (occurred_at <> ''),
    UNIQUE (run_id, invocation_id, failure_digest)
) STRICT;
CREATE TABLE attention_item_pr_references (
    item_id   TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    repo      TEXT NOT NULL CHECK (repo <> ''),
    pr_number INTEGER NOT NULL CHECK (pr_number > 0),
    body      TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE finding_dispositions (
    finding_id  TEXT NOT NULL REFERENCES findings (id),
    run_id      TEXT NOT NULL REFERENCES runs (id),
    round       INTEGER NOT NULL CHECK (round > 0),
    disposition TEXT NOT NULL CHECK (disposition IN ('fixed', 'declined', 'deferred')),
    reason      TEXT NOT NULL CHECK (reason <> ''),
    remediation_invocation_id TEXT NOT NULL,
    created_at  TEXT NOT NULL CHECK (created_at <> ''),
    body_digest TEXT NOT NULL CHECK (body_digest <> ''),
    body        TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (finding_id, round),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round),
    CHECK (
        (disposition = 'fixed' AND remediation_invocation_id <> '')
        OR
        (disposition <> 'fixed' AND remediation_invocation_id = '')
    )
) STRICT;
CREATE TABLE requirement_resolutions (
    digest                    TEXT PRIMARY KEY CHECK (digest <> ''),
    requirement_key           TEXT NOT NULL CHECK (requirement_key <> ''),
    check_class               TEXT NOT NULL CHECK (check_class IN ('clean_verification', 'independent_review', 'repo_change_policy')),
    requirement_set_digest    TEXT NOT NULL CHECK (requirement_set_digest <> ''),
    floor_registry_generation INTEGER NOT NULL CHECK (floor_registry_generation > 0),
    resolved_policy_digest    TEXT NOT NULL CHECK (resolved_policy_digest <> ''),
    body_digest               TEXT NOT NULL CHECK (body_digest <> ''),
    body                      TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE check_proofs (
    digest                        TEXT PRIMARY KEY CHECK (digest <> ''),
    requirement_resolution_digest TEXT NOT NULL REFERENCES requirement_resolutions (digest),
    candidate_head                TEXT NOT NULL CHECK (candidate_head <> ''),
    recipe_digest                 TEXT NOT NULL CHECK (recipe_digest <> ''),
    body_digest                   TEXT NOT NULL CHECK (body_digest <> ''),
    body                          TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE degraded_waivers (
    waiver_id                      TEXT PRIMARY KEY CHECK (waiver_id <> ''),
    requirement_resolution_digest TEXT NOT NULL REFERENCES requirement_resolutions (digest),
    check_class                    TEXT NOT NULL CHECK (check_class = 'repo_change_policy'),
    authority                      TEXT NOT NULL CHECK (authority IN ('explicit_human_approval', 'daemon_trusted_configuration')),
    floor_registry_generation      INTEGER NOT NULL CHECK (floor_registry_generation > 0),
    lifecycle_digest               TEXT NOT NULL CHECK (lifecycle_digest <> ''),
    body_digest                    TEXT NOT NULL CHECK (body_digest <> ''),
    body                           TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE waiver_lifecycle_events (
    waiver_id       TEXT NOT NULL REFERENCES degraded_waivers (waiver_id),
    sequence        INTEGER NOT NULL CHECK (sequence > 0),
    status          TEXT NOT NULL CHECK (status IN ('granted', 'revoked', 'expired')),
    previous_digest TEXT NOT NULL,
    event_digest    TEXT NOT NULL UNIQUE CHECK (event_digest <> ''),
    recorded_at     TEXT NOT NULL CHECK (recorded_at <> ''),
    body_digest     TEXT NOT NULL CHECK (body_digest <> ''),
    body            TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (waiver_id, sequence),
    CHECK ((sequence = 1 AND status = 'granted' AND previous_digest = '') OR
           (sequence > 1 AND previous_digest <> ''))
) STRICT;
CREATE TABLE codex_reenrollment_operations (
    auth_identity_id       TEXT NOT NULL REFERENCES auth_identities (id),
    lease_fence            INTEGER NOT NULL CHECK (lease_fence > 0),
    marker_item_id         TEXT NOT NULL REFERENCES attention_items (id),
    holder                 TEXT NOT NULL CHECK (holder <> ''),
    opened_at              TEXT NOT NULL CHECK (opened_at <> ''),
    outcome                TEXT CHECK (outcome IS NULL OR outcome IN ('failed', 'verified')),
    failure_class          TEXT CHECK (failure_class IS NULL OR failure_class IN
                              ('auth_store_replacement_failed', 'verification_failed', 'lease_lost')),
    auth_store_digest      TEXT,
    access_token_expires_at TEXT,
    completed_at           TEXT,
    body                   TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (auth_identity_id, lease_fence),
    CHECK (
        (outcome IS NULL AND failure_class IS NULL AND auth_store_digest IS NULL AND
         access_token_expires_at IS NULL AND completed_at IS NULL)
        OR
        (outcome = 'failed' AND failure_class IS NOT NULL AND auth_store_digest IS NULL AND
         access_token_expires_at IS NULL AND completed_at IS NOT NULL)
        OR
        (outcome = 'verified' AND failure_class IS NULL AND auth_store_digest IS NOT NULL AND
         auth_store_digest <> '' AND access_token_expires_at IS NOT NULL AND completed_at IS NOT NULL)
    )
) STRICT;
CREATE TABLE codex_reenrollment_recovery_transitions (
    id                      INTEGER PRIMARY KEY,
    auth_identity_id        TEXT NOT NULL REFERENCES auth_identities (id),
    lease_fence             INTEGER NOT NULL CHECK (lease_fence > 0),
    auth_store_digest       TEXT NOT NULL CHECK (auth_store_digest <> ''),
    access_token_expires_at TEXT NOT NULL CHECK (access_token_expires_at <> ''),
    command_id              TEXT REFERENCES commands (command_id)
                                 CHECK (command_id IS NULL OR command_id <> ''),
    reason                  TEXT NOT NULL CHECK (reason <> ''),
    occurred_at             TEXT NOT NULL CHECK (occurred_at <> ''),
    UNIQUE (auth_identity_id, lease_fence),
    FOREIGN KEY (auth_identity_id, lease_fence)
        REFERENCES codex_reenrollment_operations (auth_identity_id, lease_fence)
) STRICT;
CREATE TABLE effect_proposal_instances (
    instance_id             TEXT NOT NULL PRIMARY KEY CHECK (instance_id <> ''),
    admission_key           TEXT NOT NULL UNIQUE CHECK (admission_key <> ''),
    proposal_batch_id       TEXT NOT NULL CHECK (proposal_batch_id <> ''),
    effect_kind             TEXT NOT NULL CHECK (effect_kind = 'run_proposal'),
    content_digest          TEXT NOT NULL CHECK (content_digest <> ''),
    resolved_policy_run_id  TEXT NOT NULL REFERENCES resolved_policies (run_id),
    resolved_policy_digest  TEXT NOT NULL CHECK (resolved_policy_digest <> ''),
    subject_handle          TEXT NOT NULL CHECK (subject_handle <> ''),
    created_at              TEXT NOT NULL CHECK (created_at <> ''),
    body                    TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE effect_proposal_items (
    item_id         TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    instance_id     TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    content_digest  TEXT NOT NULL CHECK (content_digest <> ''),
    UNIQUE (instance_id, content_digest)
) STRICT;
CREATE TABLE effect_proposal_revisions (
    instance_id       TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    content_digest    TEXT NOT NULL CHECK (content_digest <> ''),
    supersedes_digest TEXT NOT NULL CHECK (supersedes_digest <> ''),
    command_id        TEXT NOT NULL UNIQUE REFERENCES commands (command_id),
    created_at        TEXT NOT NULL CHECK (created_at <> ''),
    body              TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (instance_id, content_digest)
) STRICT;
CREATE TABLE effect_proposal_decisions (
    instance_id     TEXT NOT NULL PRIMARY KEY REFERENCES effect_proposal_instances (instance_id),
    command_id      TEXT NOT NULL UNIQUE REFERENCES commands (command_id),
    action          TEXT NOT NULL CHECK (action IN ('start', 'start_with_changes', 'decline')),
    selected_digest TEXT,
    decided_at      TEXT NOT NULL CHECK (decided_at <> ''),
    CHECK ((action IN ('start', 'start_with_changes') AND selected_digest IS NOT NULL AND selected_digest <> '')
        OR (action = 'decline' AND selected_digest IS NULL))
) STRICT;
CREATE TABLE effect_proposal_snoozes (
    command_id   TEXT NOT NULL PRIMARY KEY REFERENCES commands (command_id),
    instance_id  TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    snooze_until TEXT NOT NULL CHECK (snooze_until <> ''),
    created_at   TEXT NOT NULL CHECK (created_at <> ''),
    released_at  TEXT
) STRICT;
CREATE TABLE intake_occurrences (
    repository_id        INTEGER NOT NULL CHECK (repository_id > 0),
    issue_number         INTEGER NOT NULL CHECK (issue_number > 0),
    label                TEXT NOT NULL CHECK (label <> ''),
    ordinal              INTEGER NOT NULL CHECK (ordinal >= 1),
    repo                 TEXT NOT NULL CHECK (repo <> ''),
    state                TEXT NOT NULL CHECK (state IN ('present', 'absent', 'closed')),
    admission_key        TEXT REFERENCES effect_proposal_instances (admission_key),
    proposal_instance_id TEXT REFERENCES effect_proposal_instances (instance_id),
    work_unit_id         TEXT REFERENCES work_unit_declarations (unit_id),
    policy_artifact_id   TEXT,
    refusal_reason       TEXT CHECK (
        refusal_reason IS NULL
        OR refusal_reason IN ('wip_cap_exhausted', 'mode_not_authorized',
                              'subject_input_missing', 'subject_input_stale')
    ),
    supersession_reason  TEXT CHECK (
        supersession_reason IS NULL
        OR supersession_reason IN ('label_removed', 'issue_closed')
    ),
    body                 TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (repository_id, issue_number, label, ordinal),
    CHECK (
        (admission_key IS NULL AND proposal_instance_id IS NULL
            AND work_unit_id IS NULL AND policy_artifact_id IS NULL)
        OR (admission_key IS NOT NULL AND proposal_instance_id IS NOT NULL
            AND work_unit_id IS NOT NULL AND policy_artifact_id IS NOT NULL)
    ),
    -- A refusal or supersession presupposes an admission (the domain invariant),
    -- so each is present only when the admission columns are.
    CHECK (refusal_reason IS NULL OR admission_key IS NOT NULL),
    CHECK (supersession_reason IS NULL OR admission_key IS NOT NULL)
) STRICT;
CREATE TABLE agent_claims (
    invocation_id  TEXT PRIMARY KEY REFERENCES agent_invocations (id),
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
) STRICT;
CREATE TABLE projects (
    project_id    TEXT NOT NULL PRIMARY KEY CHECK (project_id <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    body          TEXT NOT NULL
) STRICT;
CREATE TABLE export_rejections (
    invocation_id TEXT NOT NULL PRIMARY KEY
                       REFERENCES execution_admissions (invocation_id),
    admission_id  TEXT NOT NULL REFERENCES execution_admissions (id),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
) STRICT;
CREATE TABLE current_import_starts (
    invocation_id TEXT NOT NULL PRIMARY KEY
                       REFERENCES execution_admissions (invocation_id),
    admission_id  TEXT NOT NULL REFERENCES execution_admissions (id),
    body          TEXT NOT NULL
) STRICT;
CREATE TABLE production_attempts (
    campaign_id          TEXT    NOT NULL,
    attempt_number       INTEGER NOT NULL CHECK (attempt_number >= 1),
    kind                 TEXT    NOT NULL CHECK (kind IN ('initial', 'retry')),
    parent_run_id        TEXT,
    source_digest        TEXT    NOT NULL,
    approved_spec_digest TEXT,
    elaboration_run_id   TEXT    NOT NULL,
    implementation_run_id TEXT  NOT NULL UNIQUE,
    reason               TEXT    NOT NULL,
    as_of_revision       INTEGER NOT NULL,
    body                 TEXT    NOT NULL, publication_digest TEXT,
    PRIMARY KEY (campaign_id, attempt_number)
) STRICT;
INSERT INTO production_attempts VALUES('campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba',1,'initial',NULL,'sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403','sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2','run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','implementation-from-submit','',2,'{"campaign_id":"campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba","attempt_number":1,"kind":"initial","reason":"","source_digest":"sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","publication_digest":"sha256:f3c794222955e37bcacd199d0da87ea4f0cb721edd5a2d57b67ce9d86402219a","approved_spec_digest":"sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","elaboration_run_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","implementation_run_id":"implementation-from-submit"}','sha256:f3c794222955e37bcacd199d0da87ea4f0cb721edd5a2d57b67ce9d86402219a');
CREATE TABLE client_enrollments (
    id               TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    auth_identity_id TEXT NOT NULL REFERENCES auth_identities (id),
    harness_client   TEXT NOT NULL CHECK (harness_client <> ''),
    route            TEXT NOT NULL CHECK (route <> ''),
    auth_method      TEXT NOT NULL CHECK (auth_method <> ''),
    credential_mode  TEXT NOT NULL CHECK (credential_mode <> ''),
    refresh_strategy TEXT NOT NULL CHECK (refresh_strategy <> ''),
    supports_read_only_auth_snapshot INTEGER NOT NULL
        CHECK (supports_read_only_auth_snapshot IN (0, 1)),
    account_binding  TEXT NOT NULL CHECK (account_binding <> ''),
    recorded_at      TEXT NOT NULL CHECK (recorded_at <> ''),
    body             TEXT NOT NULL,
    UNIQUE (auth_identity_id, harness_client, route, auth_method)
) STRICT;
CREATE TABLE client_enrollment_generations (
    enrollment_id         TEXT    NOT NULL REFERENCES client_enrollments (id),
    ordinal               INTEGER NOT NULL CHECK (ordinal >= 1),
    auth_store_volume     TEXT    NOT NULL CHECK (auth_store_volume <> ''),
    store_manifest_digest TEXT    NOT NULL CHECK (store_manifest_digest <> ''),
    lease_fence           INTEGER NOT NULL CHECK (lease_fence >= 1),
    account_binding       TEXT    NOT NULL CHECK (account_binding <> ''),
    token_expiry          TEXT,
    recorded_at           TEXT    NOT NULL CHECK (recorded_at <> ''),
    body                  TEXT    NOT NULL,
    PRIMARY KEY (enrollment_id, ordinal)
) STRICT;
CREATE TABLE adapter_conformance_records (
    id             INTEGER PRIMARY KEY,
    adapter_digest TEXT NOT NULL CHECK (adapter_digest <> ''),
    outcome        TEXT NOT NULL CHECK (outcome <> ''),
    -- Canonical LaunchCapabilitySet JSON; the literal 'null' exactly when
    -- the pass did not pass (a failed pass proves nothing).
    proved_capabilities TEXT NOT NULL CHECK (proved_capabilities <> ''),
    proved_at      TEXT NOT NULL CHECK (proved_at <> '')
) STRICT;
CREATE TABLE shadow_review_records (
    invocation_id   TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id          TEXT NOT NULL REFERENCES runs (id),
    shadowed_round  INTEGER NOT NULL CHECK (shadowed_round > 0),
    source          TEXT NOT NULL CHECK (source <> ''),
    provider        TEXT NOT NULL CHECK (provider <> ''),
    base_sha        TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha        TEXT NOT NULL CHECK (head_sha <> ''),
    outcome         TEXT NOT NULL CHECK (outcome IN ('clean', 'findings')),
    completed_at    TEXT NOT NULL CHECK (completed_at <> ''),
    body_digest     TEXT NOT NULL CHECK (body_digest <> ''),
    body            TEXT NOT NULL CHECK (body <> ''),
    UNIQUE (run_id, shadowed_round, source),
    UNIQUE (invocation_id, run_id)
) STRICT;
CREATE TABLE shadow_review_record_findings (
    invocation_id TEXT NOT NULL,
    finding_id    TEXT NOT NULL REFERENCES findings (id),
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    PRIMARY KEY (invocation_id, finding_id),
    UNIQUE (finding_id),
    UNIQUE (invocation_id, ordinal),
    FOREIGN KEY (invocation_id) REFERENCES shadow_review_records (invocation_id)
) STRICT;
CREATE TABLE classifier_accuracy_samples (
    run_id                  TEXT NOT NULL REFERENCES runs (id),
    finding_id              TEXT NOT NULL,
    classification_version INTEGER NOT NULL CHECK (classification_version > 0),
    shadow_invocation_id    TEXT NOT NULL,
    assessment              TEXT NOT NULL CHECK (assessment <> ''),
    recorded_at             TEXT NOT NULL CHECK (recorded_at <> ''),
    body_digest             TEXT NOT NULL CHECK (body_digest <> ''),
    body                    TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (shadow_invocation_id, finding_id, classification_version),
    FOREIGN KEY (finding_id, classification_version)
        REFERENCES classifications (finding_id, version),
    FOREIGN KEY (shadow_invocation_id, run_id)
        REFERENCES shadow_review_records (invocation_id, run_id),
    FOREIGN KEY (shadow_invocation_id, finding_id)
        REFERENCES shadow_review_record_findings (invocation_id, finding_id)
) STRICT;
CREATE TABLE shadow_review_configuration_approvals (
    approval_digest TEXT PRIMARY KEY CHECK (approval_digest <> ''),
    repo TEXT NOT NULL CHECK (repo <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    source TEXT NOT NULL CHECK (source <> ''),
    configuration_digest TEXT NOT NULL CHECK (configuration_digest <> ''),
    recorded_at TEXT NOT NULL CHECK (recorded_at <> ''),
    body TEXT NOT NULL CHECK (body <> ''),
    UNIQUE (
        approval_digest, repo, repository_id, source, configuration_digest
    )
);
CREATE TABLE shadow_review_configuration_activations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo TEXT NOT NULL CHECK (repo <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    source TEXT NOT NULL CHECK (source <> ''),
    approval_digest TEXT NOT NULL CHECK (approval_digest <> ''),
    configuration_digest TEXT NOT NULL CHECK (configuration_digest <> ''),
    activated_at TEXT NOT NULL CHECK (activated_at <> ''),
    FOREIGN KEY (
        approval_digest, repo, repository_id, source, configuration_digest
    ) REFERENCES shadow_review_configuration_approvals (
        approval_digest, repo, repository_id, source, configuration_digest
    )
);
CREATE TABLE IF NOT EXISTS "finding_adjudications" (
    run_id                      TEXT    NOT NULL REFERENCES runs (id),
    round                       INTEGER NOT NULL CHECK (round > 0),
    revision                    INTEGER NOT NULL CHECK (revision > 0),
    predecessor_digest          TEXT,
    content_digest              TEXT    NOT NULL CHECK (content_digest <> ''),
    finding_batch_digest        TEXT    NOT NULL CHECK (finding_batch_digest <> ''),
    approved_spec_digest        TEXT    NOT NULL CHECK (approved_spec_digest <> ''),
    instruction_snapshot_digest TEXT    NOT NULL CHECK (instruction_snapshot_digest <> ''),
    resolved_policy_digest      TEXT    NOT NULL CHECK (resolved_policy_digest <> ''),
    created_at                  TEXT    NOT NULL,
    body_digest                 TEXT    NOT NULL,
    body                        TEXT    NOT NULL,
    CHECK ((revision = 1) = (predecessor_digest IS NULL)),
    PRIMARY KEY (run_id, round, revision),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round)
) STRICT;
CREATE TABLE attention_decision_surfaces (
    item_id TEXT    NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    epoch   INTEGER NOT NULL CHECK (epoch > 0),
    digest  TEXT    NOT NULL CHECK (digest <> ''),
    body    TEXT    NOT NULL CHECK (body <> '')
) STRICT;
INSERT INTO attention_decision_surfaces VALUES('spec-approval-implementation-from-submit-1',1,'sha256:1da37365a32af17b5fd4eebcb60ee5de5b318f93b5f11e53acec679bb10d6b94','{"item_id":"spec-approval-implementation-from-submit-1","epoch":1,"subject":{"subject_type":"run","subject_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","run_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160"},"requested_decision":["approve","discuss","request_changes","stop"],"pr_head_sha":"","presented_artifact_digests":["sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","sha256:8e714f53a1974d5baffab99aff7e83fcd8f96d5ffc36c13b73fea0bed9b16dfe"],"digest":"sha256:1da37365a32af17b5fd4eebcb60ee5de5b318f93b5f11e53acec679bb10d6b94"}');
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
CREATE TABLE usage_observations (
    invocation_id    TEXT    NOT NULL REFERENCES execution_admissions (invocation_id),
    run_id           TEXT    NOT NULL REFERENCES runs (id),
    agent_digest     TEXT    NOT NULL CHECK (agent_digest <> ''),
    launch_digest    TEXT    NOT NULL CHECK (launch_digest <> ''),
    treatment_digest TEXT    NOT NULL CHECK (treatment_digest <> ''),
    pricing_revision TEXT    NOT NULL CHECK (pricing_revision <> ''),
    source           TEXT    NOT NULL CHECK (source <> ''),
    kind             TEXT    NOT NULL CHECK (kind <> ''),
    metric           TEXT    NOT NULL CHECK (metric <> ''),
    unit             TEXT    NOT NULL CHECK (unit <> ''),
    quantity         INTEGER NOT NULL CHECK (quantity >= 0),
    sequence         INTEGER NOT NULL CHECK (sequence > 0),
    observed_at      TEXT    NOT NULL CHECK (observed_at <> ''),
    PRIMARY KEY (invocation_id, source, kind, metric, sequence)
) STRICT;
CREATE TRIGGER execution_outcomes_exclude_exports
BEFORE INSERT ON execution_outcomes
WHEN EXISTS (
    SELECT 1 FROM execution_exports
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution outcome conflicts with existing export');
END;
CREATE TRIGGER execution_exports_exclude_outcomes
BEFORE INSERT ON execution_exports
WHEN EXISTS (
    SELECT 1 FROM execution_outcomes
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution export conflicts with existing outcome');
END;
CREATE TRIGGER execution_outcomes_exclude_exports_on_update
BEFORE UPDATE OF invocation_id ON execution_outcomes
WHEN EXISTS (
    SELECT 1 FROM execution_exports
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution outcome conflicts with existing export');
END;
CREATE TRIGGER execution_exports_exclude_outcomes_on_update
BEFORE UPDATE OF invocation_id ON execution_exports
WHEN EXISTS (
    SELECT 1 FROM execution_outcomes
    WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution export conflicts with existing outcome');
END;
CREATE TRIGGER review_record_rejects_failure
BEFORE INSERT ON review_records
WHEN EXISTS (
    SELECT 1 FROM review_failures
    WHERE invocation_id = NEW.invocation_id
       OR (run_id = NEW.run_id AND round = NEW.round)
)
BEGIN
    SELECT RAISE(ABORT, 'review invocation already failed');
END;
CREATE TRIGGER review_failure_rejects_record
BEFORE INSERT ON review_failures
WHEN EXISTS (
    SELECT 1 FROM review_records
    WHERE invocation_id = NEW.invocation_id
       OR (run_id = NEW.run_id AND round = NEW.round)
)
BEGIN
    SELECT RAISE(ABORT, 'review invocation already completed');
END;
CREATE TRIGGER finding_disposition_requires_round_finding
BEFORE INSERT ON finding_dispositions
WHEN NOT EXISTS (
    SELECT 1
    FROM review_records AS record
    JOIN review_record_findings AS finding
      ON finding.invocation_id = record.invocation_id
    JOIN findings AS raw_finding
      ON raw_finding.id = finding.finding_id
     AND raw_finding.run_id = record.run_id
    JOIN json_each(record.body, '$.finding_ids') AS body_finding
      ON body_finding.value = finding.finding_id
    WHERE record.run_id = NEW.run_id
      AND record.round = NEW.round
      AND finding.finding_id = NEW.finding_id
      AND json_extract(record.body, '$.run_id') = record.run_id
      AND json_extract(record.body, '$.round') = record.round
      AND (NEW.disposition <> 'fixed' OR EXISTS (
          SELECT 1
          FROM review_records AS remediation
          WHERE remediation.invocation_id = NEW.remediation_invocation_id
            AND remediation.run_id = record.run_id
            AND remediation.round > record.round
            AND remediation.base_sha = record.base_sha
            AND remediation.head_sha <> record.head_sha
            AND json_extract(remediation.body, '$.run_id') = remediation.run_id
            AND json_extract(remediation.body, '$.round') = remediation.round
            AND json_extract(remediation.body, '$.base_sha') = remediation.base_sha
            AND json_extract(remediation.body, '$.head_sha') = remediation.head_sha
      ))
      AND json_extract(raw_finding.body, '$.id') = raw_finding.id
      AND json_extract(raw_finding.body, '$.run_id') = raw_finding.run_id
)
BEGIN
    SELECT RAISE(ABORT, 'finding does not belong to review round');
END;
CREATE TRIGGER outbox_publication_intent_requires_current_insert
BEFORE INSERT ON outbox
WHEN NEW.kind = 'publish.publication' AND NEW.payload_version != 2
BEGIN
    SELECT RAISE(ABORT, 'new publication intents require current payload version');
END;
CREATE TRIGGER outbox_publication_intent_requires_current_promotion
BEFORE UPDATE OF kind, payload_version ON outbox
WHEN NEW.kind = 'publish.publication'
    AND OLD.kind != 'publish.publication'
    AND NEW.payload_version != 2
BEGIN
    SELECT RAISE(ABORT, 'promoted publication intents require current payload version');
END;
CREATE TRIGGER outbox_payload_version_no_downgrade
BEFORE UPDATE OF payload_version ON outbox
WHEN NEW.payload_version < OLD.payload_version
BEGIN
    SELECT RAISE(ABORT, 'outbox payload version cannot be downgraded');
END;
CREATE TRIGGER shadow_review_rejects_routed_invocation
BEFORE INSERT ON shadow_review_records
WHEN EXISTS (
    SELECT 1 FROM review_records WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_failures WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_retries WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review invocation cannot enter shadow review');
END;
CREATE TRIGGER shadow_review_update_rejects_routed_invocation
BEFORE UPDATE OF invocation_id ON shadow_review_records
WHEN EXISTS (
    SELECT 1 FROM review_records WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_failures WHERE invocation_id = NEW.invocation_id
    UNION ALL
    SELECT 1 FROM review_retries WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review invocation cannot enter shadow review');
END;
CREATE TRIGGER routed_review_rejects_shadow_invocation
BEFORE INSERT ON review_records
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter routed review');
END;
CREATE TRIGGER routed_review_update_rejects_shadow_invocation
BEFORE UPDATE OF invocation_id ON review_records
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter routed review');
END;
CREATE TRIGGER review_failure_rejects_shadow_invocation
BEFORE INSERT ON review_failures
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review failure');
END;
CREATE TRIGGER review_failure_update_rejects_shadow_invocation
BEFORE UPDATE OF invocation_id ON review_failures
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review failure');
END;
CREATE TRIGGER review_retry_rejects_shadow_invocation
BEFORE INSERT ON review_retries
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review retry');
END;
CREATE TRIGGER review_retry_update_rejects_shadow_invocation
BEFORE UPDATE OF invocation_id ON review_retries
WHEN EXISTS (
    SELECT 1 FROM shadow_review_records WHERE invocation_id = NEW.invocation_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review invocation cannot enter review retry');
END;
CREATE TRIGGER routed_review_finding_rejects_shadow
BEFORE INSERT ON review_record_findings
WHEN EXISTS (
    SELECT 1 FROM shadow_review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review finding cannot enter routed review');
END;
CREATE TRIGGER routed_review_finding_update_rejects_shadow
BEFORE UPDATE OF finding_id ON review_record_findings
WHEN EXISTS (
    SELECT 1 FROM shadow_review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'shadow review finding cannot enter routed review');
END;
CREATE TRIGGER shadow_review_finding_rejects_routed
BEFORE INSERT ON shadow_review_record_findings
WHEN EXISTS (
    SELECT 1 FROM review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review finding cannot enter shadow review');
END;
CREATE TRIGGER shadow_review_finding_update_rejects_routed
BEFORE UPDATE OF finding_id ON shadow_review_record_findings
WHEN EXISTS (
    SELECT 1 FROM review_record_findings
    WHERE finding_id = NEW.finding_id
)
BEGIN
    SELECT RAISE(ABORT, 'routed review finding cannot enter shadow review');
END;
CREATE TRIGGER usage_observations_append_only_update
BEFORE UPDATE ON usage_observations
BEGIN
    SELECT RAISE(ABORT, 'usage observations are append-only');
END;
CREATE TRIGGER usage_observations_append_only_delete
BEFORE DELETE ON usage_observations
BEGIN
    SELECT RAISE(ABORT, 'usage observations are append-only');
END;
CREATE INDEX execution_admissions_run ON execution_admissions (run_id);
CREATE INDEX project_images_repository ON project_images (repository_id);
CREATE INDEX attention_items_open_by_type
    ON attention_items (item_type) WHERE status = 'open';
CREATE INDEX backend_conformance_by_backend
    ON backend_conformance_records (backend, id);
CREATE INDEX trust_profile_activations_repo_id
    ON trust_profile_activations(repo, id);
CREATE UNIQUE INDEX run_milestones_identity
    ON run_milestones (run_id, kind, COALESCE(invocation_id, ''));
CREATE INDEX invocation_observations_by_run ON invocation_observations (run_id);
CREATE INDEX schedule_timers_due ON schedule_timers (next_nominal_fire_at);
CREATE UNIQUE INDEX schedule_occurrences_identity
    ON schedule_occurrences (schedule_id, generation, nominal_fire_at);
CREATE INDEX schedule_occurrences_pending
    ON schedule_occurrences (status, id);
CREATE INDEX pull_merge_facts_resource
    ON pull_merge_facts (repository_id, pr_number, id);
CREATE INDEX issue_state_facts_resource
    ON issue_state_facts (repository_id, issue_number, id);
CREATE INDEX schedules_due ON schedules (status, fire_at);
CREATE INDEX review_records_by_candidate
    ON review_records (run_id, base_sha, head_sha, round DESC);
CREATE INDEX review_failures_by_run
    ON review_failures (run_id, round DESC);
CREATE INDEX native_review_observations_by_identity
    ON native_review_observations (repository_id, pr_number, provider, kind, native_id, id);
CREATE INDEX review_recovery_transitions_by_run
    ON review_recovery_transitions (run_id, id DESC);
CREATE INDEX review_configuration_recovery_transitions_by_run
    ON review_configuration_recovery_transitions (run_id, id DESC);
CREATE INDEX finding_dispositions_by_run
    ON finding_dispositions (run_id, round, finding_id);
CREATE INDEX requirement_resolutions_by_set_key
    ON requirement_resolutions (requirement_set_digest, requirement_key);
CREATE INDEX codex_reenrollment_operations_latest
    ON codex_reenrollment_operations (auth_identity_id, lease_fence DESC);
CREATE INDEX codex_reenrollment_recovery_transitions_latest
    ON codex_reenrollment_recovery_transitions (auth_identity_id, id DESC);
CREATE INDEX effect_proposal_instances_batch
    ON effect_proposal_instances (proposal_batch_id, instance_id);
CREATE INDEX effect_proposal_snoozes_instance
    ON effect_proposal_snoozes (instance_id, created_at);
CREATE INDEX execution_admissions_auth_identity
    ON execution_admissions (auth_identity_id, invocation_id);
CREATE INDEX attention_items_open_by_run
    ON attention_items (subject_run_id) WHERE status = 'open';
CREATE UNIQUE INDEX auth_identities_account_binding
    ON auth_identities (account_binding) WHERE account_binding <> '';
CREATE INDEX adapter_conformance_by_adapter
    ON adapter_conformance_records (adapter_digest, id);
CREATE INDEX shadow_review_records_by_candidate
    ON shadow_review_records (run_id, base_sha, head_sha, shadowed_round, source);
CREATE INDEX classifier_accuracy_samples_by_run
    ON classifier_accuracy_samples (run_id, recorded_at, shadow_invocation_id);
CREATE INDEX shadow_review_configuration_approvals_repo_source
ON shadow_review_configuration_approvals (repo, source);
CREATE INDEX shadow_review_configuration_activations_current
ON shadow_review_configuration_activations (repo, source, id DESC);
CREATE INDEX finding_adjudications_by_digest
    ON finding_adjudications (content_digest);
CREATE INDEX usage_observations_run
    ON usage_observations (run_id);
CREATE INDEX usage_observations_treatment
    ON usage_observations (treatment_digest);
COMMIT;
