-- A database the pre-rename daemon wrote at schema 63, migrated to 146 by
-- b7bd02f71538ef66414ee1874ef378ca9dbf3a0f (the last build before the
-- migration baseline, #3252) and dumped with sqlite3 .dump. Its rows still
-- spell the pre-rename vocabulary; only the migrations ran over them.
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
INSERT INTO schema_migrations VALUES(64,'0064_specification_vocabulary.sql','sha256:10e803da6dbadc3fe88712842363beb9be0efa86a41dfbbd9b2add25c6eb6d26','2026-11-16T19:44:16.100429Z');
INSERT INTO schema_migrations VALUES(65,'0065_comprehension_telemetry.sql','sha256:df9b8a2f66e09c240f132dce1106f34978e52cab5c0d60675e509a9d649e1aaa','2026-11-16T19:44:16.102227Z');
INSERT INTO schema_migrations VALUES(66,'0066_work_unit_completed_milestone.sql','sha256:715f6731ff44bc050e0eb04922a15f967eaca90b8f6b37255a664bf07b00a413','2026-11-16T19:44:16.109614Z');
INSERT INTO schema_migrations VALUES(67,'0067_review_requests.sql','sha256:524e5ed51bb3d8cd98ad544df16ee069281d48c57683ce842f63097832a487b0','2026-11-16T19:44:16.110941Z');
INSERT INTO schema_migrations VALUES(68,'0068_declared_publication_branch.sql','sha256:50335112242294a37b196d7c052d5767a249c0f173394c7c1a8841e0b15a682c','2026-11-16T19:44:16.117084Z');
INSERT INTO schema_migrations VALUES(69,'0069_successor_publication.sql','sha256:f0c4e9d72bf55bd8a87d49c38df396ef2335263f4a902ce31c00a63310ea2fff','2026-11-16T19:44:16.129104Z');
INSERT INTO schema_migrations VALUES(70,'0070_tasks.sql','sha256:fa3375de106018734407391a32ebf321118df788cd48b1d0fe82ca8f9d8582ce','2026-11-16T19:44:16.139257Z');
INSERT INTO schema_migrations VALUES(71,'0071_production_attempt_revision_link.sql','sha256:e57d6c4ef30e7a59ee7dc4a5b45c552404c4929c02af4ccdc842e0efc55e442a','2026-11-16T19:44:16.143592Z');
INSERT INTO schema_migrations VALUES(72,'0072_task_lifecycle_facts.sql','sha256:513dc75d536d1bfc96295b2c06093bb62c75cdc2ab15f76dbeab01e28f898eba','2026-11-16T19:44:16.144981Z');
INSERT INTO schema_migrations VALUES(73,'0073_task_submission_commands.sql','sha256:5c734e94c34bc4cfaaa240cd7f6b138290c54b035dbe2c63b9dac96a88866edf','2026-11-16T19:44:16.145812Z');
INSERT INTO schema_migrations VALUES(74,'0074_task_proposal_vocabulary.sql','sha256:0d03a4b609f615899ac705f910c72cba266cb526ea1d9680e7abc7abefd6bf9d','2026-11-16T19:44:16.146341Z');
INSERT INTO schema_migrations VALUES(75,'0075_manual_submissions.sql','sha256:6ebb400b724f76e696e74f2965a09eba3d2c5c3b38496f752340d4ebd23bdafb','2026-11-16T19:44:16.148792Z');
INSERT INTO schema_migrations VALUES(76,'0076_task_cancellation.sql','sha256:67c702f4ab0fc91555cf752dbe39ee7ff22197b54f93374bf369699554b26100','2026-11-16T19:44:16.149813Z');
INSERT INTO schema_migrations VALUES(77,'0077_source_issue_closure_kind.sql','sha256:8b149b0dc6cda5f59b012aaf41c855b946319c2661bfd04f170a063e18163d4d','2026-11-16T19:44:16.151142Z');
INSERT INTO schema_migrations VALUES(78,'0078_publication_authorings.sql','sha256:dbd64a262a42607c38c4e1e8adc616d021cf5b6eeaef6376b1e305809455a5aa','2026-11-16T19:44:16.152178Z');
INSERT INTO schema_migrations VALUES(79,'0079_effect_proposal_approvals.sql','sha256:94f5dfcae755be38a846cba5ef33cdc61e1fcd1cd76477c2891ba337a65f85d5','2026-11-16T19:44:16.153853Z');
INSERT INTO schema_migrations VALUES(80,'0080_effect_proposal_policy_approvals.sql','sha256:eac3466c3e7fbf16f32b06a40cf6db067901da633d8fa74b7f3770a60e5a22e5','2026-11-16T19:44:16.155227Z');
INSERT INTO schema_migrations VALUES(81,'0081_project_authority_backfill.sql','sha256:a6a47a75d500b473ab31d0e5572eb654e2b771e0c047b81de87d094b72f972b1','2026-11-16T19:44:16.157993Z');
INSERT INTO schema_migrations VALUES(82,'0082_auth_store_read_holds.sql','sha256:7dbf10c343a1f0ebf239ce8ffd458a7d334223c072d8ab3ce4ee12e1942b2bb7','2026-11-16T19:44:16.165469Z');
INSERT INTO schema_migrations VALUES(83,'0083_review_round_diff_metrics.sql','sha256:da070756b41e8c1253d0a6b5f7c51e4e8ea36df594900412d67627c64e81fc2b','2026-11-16T19:44:16.166676Z');
INSERT INTO schema_migrations VALUES(84,'0084_drift_audits.sql','sha256:b0fa63d506ae4ba6c7c2cb8b8b2345b78de8f633e655e31a7665010ab8293e44','2026-11-16T19:44:16.167722Z');
INSERT INTO schema_migrations VALUES(85,'0085_finding_disposition_supersessions.sql','sha256:e279cdec93a1fcfa496495574aef3a90cbbd16a6952d339970c39e9dcff94c29','2026-11-16T19:44:16.168586Z');
INSERT INTO schema_migrations VALUES(86,'0086_held_item_pr_bindings.sql','sha256:573eed56e004e6600eb3dd120aacc939c7014033d7207cdbf11125573d47cbe5','2026-11-16T19:44:16.169423Z');
INSERT INTO schema_migrations VALUES(87,'0087_generation_integrity_marks.sql','sha256:4cea0627305e0b35625784cefb34bb32241aa482c2d0146c5f4f991fe544a764','2026-11-16T19:44:16.170158Z');
INSERT INTO schema_migrations VALUES(88,'0088_follow_up_filing_kind.sql','sha256:0ad748c0108eba5bc7d8148dce63ef2bd4ac82e978c0a03e5135519a616bfe47','2026-11-16T19:44:16.171359Z');
INSERT INTO schema_migrations VALUES(89,'0089_follow_up_filing_ledger.sql','sha256:a4ed183b8a3d7768e9b672018b0984144f602d411c7e945ad9993700bdadc027','2026-11-16T19:44:16.173194Z');
INSERT INTO schema_migrations VALUES(90,'0090_external_finding_dispositions.sql','sha256:2fb214d6b230152bfb56e4346e42c5fea26471be0cd9902daae815c87c94a89b','2026-11-16T19:44:16.174462Z');
INSERT INTO schema_migrations VALUES(91,'0091_project_image_environment.sql','sha256:5635908cb530f9714b8f63ff5909b13d908c3cd252edc7106646204194c009ad','2026-11-16T19:44:16.178369Z');
INSERT INTO schema_migrations VALUES(92,'0092_publish_audit_issues.sql','sha256:1a233e5d3e6822a42688e02f55c34ee6985e9415d83d76d00898ac24c12384c1','2026-11-16T19:44:16.183137Z');
INSERT INTO schema_migrations VALUES(93,'0093_follow_up_filing_dispatch_identity.sql','sha256:28bacbb3649c1fef14fcce283637ba0d63ae4192d47ee6d02361846fe9c8e7b0','2026-11-16T19:44:16.186111Z');
INSERT INTO schema_migrations VALUES(94,'0094_device_activity.sql','sha256:ad700201159723bc3fe27bc6e9bd1627acfd3b784ed68388dbe6d46615dc8b20','2026-11-16T19:44:16.187022Z');
INSERT INTO schema_migrations VALUES(95,'0095_task_lines.sql','sha256:cabf23a0b2f56b4292f62c591235a4725d02d77de08133bbaa3022ee76badabd','2026-11-16T19:44:16.18786Z');
INSERT INTO schema_migrations VALUES(96,'0096_retire_blocked_items.sql','sha256:7e94629ab0e6cdd330c63523de74cc56710c5fe7f495353656a1e282d51f23a7','2026-11-16T19:44:16.188359Z');
INSERT INTO schema_migrations VALUES(97,'0097_review_admissions.sql','sha256:dc488336ef46c31cc6de7bc93abb690e2ef995fe53bdcce37c45371e2f30e14a','2026-11-16T19:44:16.189159Z');
INSERT INTO schema_migrations VALUES(98,'0098_review_dispute_rows.sql','sha256:435039787b077c0b23656edf2160037ed6319d4c34c2514d11029eda1b4065f1','2026-11-16T19:44:16.189924Z');
INSERT INTO schema_migrations VALUES(99,'0099_publish_installation_mint_holds.sql','sha256:e78db2a463f6a8f5b0d91a9ab49702261a03a810982130decabbc4e848cd3ae2','2026-11-16T19:44:16.190623Z');
INSERT INTO schema_migrations VALUES(100,'0100_task_workflow_closures.sql','sha256:f112eac21fd4c024c1b07aff350d7ec41997e3c6c42f32d7b67155e93e5303fb','2026-11-16T19:44:16.19142Z');
INSERT INTO schema_migrations VALUES(101,'0101_work_unit_rebase_proofs.sql','sha256:aeae7a2e2567298523bd7c484a6f652058f1babb021ee79fcb1469106b951ad6','2026-11-16T19:44:16.192242Z');
INSERT INTO schema_migrations VALUES(102,'0102_usage_observations_review.sql','sha256:05376ae1fc4e9ad6b93166bd75893287d0d2a38626d3a241a2dc35748efdd617','2026-11-16T19:44:16.20608Z');
INSERT INTO schema_migrations VALUES(103,'0103_run_hold_admission_detail.sql','sha256:e25eda53a1dde34e846767df8915f4017a75ce7a1e6fa019bcb733007d2d26ba','2026-11-16T19:44:16.234164Z');
INSERT INTO schema_migrations VALUES(104,'0104_execution_outcome_cause.sql','sha256:c2117420a317c59ad69484517de0428ec6e222e7f1ecfdd16e21dd7187a01711','2026-11-16T19:44:16.237366Z');
INSERT INTO schema_migrations VALUES(105,'0105_admitted_image_pointers.sql','sha256:775fe677453686e597e0d5cd2b507c75e4b4f033069c8dac63c76a56738f4b9c','2026-11-16T19:44:16.238753Z');
INSERT INTO schema_migrations VALUES(106,'0106_schedules_image_upkeep.sql','sha256:6a6be3768012218f61574c4a6d33e75c1f8b96ed1215456002ba78f89adef0cf','2026-11-16T19:44:16.247102Z');
INSERT INTO schema_migrations VALUES(107,'0107_run_hold_image_upkeep.sql','sha256:cf4b6bbc9941c51b64c56ba69daa7710e0d4a32a56d963b66b9062d247d585c3','2026-11-16T19:44:16.257159Z');
INSERT INTO schema_migrations VALUES(108,'0108_image_upkeep.sql','sha256:49279ffc8cc8496f19d2efc9c977ad7275844a1941883468a7e18b7371c841ff','2026-11-16T19:44:16.258499Z');
INSERT INTO schema_migrations VALUES(109,'0109_task_submission_origin.sql','sha256:681c332f35420472dbd84f1cd5ccea13e2e98cf69801583fd642cf129171664f','2026-11-16T19:44:16.269337Z');
INSERT INTO schema_migrations VALUES(110,'0110_image_policy_adoption.sql','sha256:dbaedfe2d6c2489ca37177fa44a5dcef9440a48ea88172039cafe6d81502a20c','2026-11-16T19:44:16.270955Z');
INSERT INTO schema_migrations VALUES(111,'0111_task_line_provenance.sql','sha256:bbba96f93904bc2f83695e396c9e27a57b6ab9c38fb37c12305aa875f66b37cd','2026-11-16T19:44:16.280877Z');
INSERT INTO schema_migrations VALUES(112,'0112_usage_pools.sql','sha256:06a3943edb4a878c0e7fbdb3f948d901eec33172bbaf2594cc5a6203ac2af39d','2026-11-16T19:44:16.297775Z');
INSERT INTO schema_migrations VALUES(113,'0113_task_line_changes.sql','sha256:913a09d3476f016c0bf525decfb1775a1e971641b4a3762de0b40f6eb08d5d2f','2026-11-16T19:44:16.307059Z');
INSERT INTO schema_migrations VALUES(114,'0114_review_slot_releases.sql','sha256:fc787c416866e4461e03af50b9b4cc1d1857d972bb999eefc271b2b8286d3c46','2026-11-16T19:44:16.3081Z');
INSERT INTO schema_migrations VALUES(115,'0115_routing_policies.sql','sha256:ff98927ffb6bc5cbc883e9dccacd43729731475761cc8a7fcbcb81ca10614c83','2026-11-16T19:44:16.308857Z');
INSERT INTO schema_migrations VALUES(116,'0116_pool_usage_observations.sql','sha256:7476731b581276694e175cfab0c72960e069617d8091b3de07889631974cf28a','2026-11-16T19:44:16.309707Z');
INSERT INTO schema_migrations VALUES(117,'0117_manual_submission_task_lines.sql','sha256:715426097cc839885321ff312ce4317ee61588b7704d05cfc8cdae3a21ae82a9','2026-11-16T19:44:16.317793Z');
INSERT INTO schema_migrations VALUES(118,'0118_usage_operator_contract.sql','sha256:61d5b038629ba90580458aaae042e39a2419d15b6c05226a9f895bc9445fa10a','2026-11-16T19:44:16.325185Z');
INSERT INTO schema_migrations VALUES(119,'0119_run_hold_quota_window.sql','sha256:665e7156290f145227b59c7f4338ccb75a9aabdd8121cc81085543030796ad7c','2026-11-16T19:44:16.347313Z');
INSERT INTO schema_migrations VALUES(120,'0120_launch_reservations.sql','sha256:c8f623a1d49bf515f291b0bb93ac3e4bc53b474c1758904fbb1a3590587de4ef','2026-11-16T19:44:16.366239Z');
INSERT INTO schema_migrations VALUES(121,'0121_routing_policy_drain_first.sql','sha256:ef30cee5f1295cdb47241871a7571ba7cc90be389a92e035a9c85e065eaf088b','2026-11-16T19:44:16.379182Z');
INSERT INTO schema_migrations VALUES(122,'0122_memory_budget.sql','sha256:b692253ef94f60c80e5c6e33d4433dd59c48b4e5b3d70f59ee90c55488a9e6b2','2026-11-16T19:44:16.380269Z');
INSERT INTO schema_migrations VALUES(123,'0123_task_triages.sql','sha256:778b2db35fdc3d5251d183439c52672c931ce40ecf65edfa31ac3a41ebadf6d7','2026-11-16T19:44:16.381194Z');
INSERT INTO schema_migrations VALUES(124,'0124_task_attachments.sql','sha256:11e68b1c22259e1695e28e289116d1ba8d56a8fabd1151958e3c0a9ffb4c24fc','2026-11-16T19:44:16.382088Z');
INSERT INTO schema_migrations VALUES(125,'0125_alternate_agent_retries.sql','sha256:2fffdf7d0275426e6a3c9ccca2b5e95c4ba45e9df16201ae89d4b7854973108b','2026-11-16T19:44:16.407196Z');
INSERT INTO schema_migrations VALUES(126,'0126_publication_handoff_checks.sql','sha256:a4b87e884ee4bd40530c573dbaa5662ab61b9aee5eeb0b8d34f181caa4b940c8','2026-11-16T19:44:16.408372Z');
INSERT INTO schema_migrations VALUES(127,'0127_discovered_model_catalogs.sql','sha256:5ffbee686462e1052e25f2ee5383c0e3da2ecb51c576783de00ac7f0ecb18192','2026-11-16T19:44:16.4093Z');
INSERT INTO schema_migrations VALUES(128,'0128_retire_base_advance_watch.sql','sha256:6ea249305ceab9081f61c130ad8235b139a3338ec93254d5f5ad15010d6f8c3f','2026-11-16T19:44:16.410032Z');
INSERT INTO schema_migrations VALUES(129,'0129_bind_handoff_check_to_audit.sql','sha256:f44cfe13f0631c38e8af7bba99e096dec7d708da47a286d916d85d713fde607f','2026-11-16T19:44:16.417058Z');
INSERT INTO schema_migrations VALUES(130,'0130_task_line_selection.sql','sha256:bc2e56df3b85126e91385be4afc5b2ff4efaf258106811fa8e09618f38f32f24','2026-11-16T19:44:16.427146Z');
INSERT INTO schema_migrations VALUES(131,'0131_device_notification_preferences.sql','sha256:18bbb87302769657715a4570694bcd1ae739ae65e396241e7a714a118a8b2c48','2026-11-16T19:44:16.42816Z');
INSERT INTO schema_migrations VALUES(132,'0132_attention_item_views.sql','sha256:d15571964126d3db8cc68aa62ff5bfb3346ce0854e5a71b75035078888a1a5e4','2026-11-16T19:44:16.429096Z');
INSERT INTO schema_migrations VALUES(133,'0133_launch_wait_keepalive.sql','sha256:ad3eef47b20686a55169163050a725c1a57a3854c0d4feb4b1c56e18fba0473a','2026-11-16T19:44:16.436464Z');
INSERT INTO schema_migrations VALUES(134,'0134_run_hold_unserved_detail.sql','sha256:77673661e1d8b62a4bb587927efafd1bbd5c267767a5803985ba4204581e8961','2026-11-16T19:44:16.44222Z');
INSERT INTO schema_migrations VALUES(135,'0135_update_drain.sql','sha256:d9e8b70ebe3f7b230b98c20a04ec7cd3e9727b80fd14223448335512202eca34','2026-11-16T19:44:16.443141Z');
INSERT INTO schema_migrations VALUES(136,'0136_publish_audit_workflows.sql','sha256:cf56edf33ec16e5b31008471c61185def04eb2796738baad2952411679bf75d0','2026-11-16T19:44:16.45209Z');
INSERT INTO schema_migrations VALUES(137,'0137_duplicate_searches.sql','sha256:c3f1d23c3d4c87ed4a928fffb7cb4826c82b4bb1279e0c64e09a31a5aa1d6fc8','2026-11-16T19:44:16.453543Z');
INSERT INTO schema_migrations VALUES(138,'0138_workflow_policy_revisions.sql','sha256:65b05409decf568633dc5cad0f9e605f461f0534ae0e157e5c8165a1a788d06a','2026-11-16T19:44:16.455068Z');
INSERT INTO schema_migrations VALUES(139,'0139_issue_comment_ledger.sql','sha256:ce09f15bb77b7a279626b4d3324f4b99894b04f4363cf45194c0d8b64d84bb05','2026-11-16T19:44:16.457332Z');
INSERT INTO schema_migrations VALUES(140,'0140_hand_back_external_dispositions.sql','sha256:ee74d8762ecc918c5ea1ffd905ac3c294d420d13ed7b88548478c4d1b8cd9ead','2026-11-16T19:44:16.458644Z');
INSERT INTO schema_migrations VALUES(141,'0141_issue_comment_policy_approvals.sql','sha256:2d074637d7b85277a2f59731ea4f890d70c510ccbf4532003561b75d6a5a0394','2026-11-16T19:44:16.459549Z');
INSERT INTO schema_migrations VALUES(142,'0142_settings_revisions.sql','sha256:694af6ad1b533f7cb021b39d20789ff1bdf677d693b8a571bb676e2e3b68e466','2026-11-16T19:44:16.460465Z');
INSERT INTO schema_migrations VALUES(143,'0143_usage_pool_call_limit.sql','sha256:ba8a2af0d05776bbebbfaa053c0bc2a654ae0218b75c3b65ed5208e90cbd9051','2026-11-16T19:44:16.461096Z');
INSERT INTO schema_migrations VALUES(144,'0144_daemon_reservations.sql','sha256:3f56e0e0155d0f62f26d62eff5e8d1c8ece783fcae110d590bde9b680ebd221b','2026-11-16T19:44:16.484858Z');
INSERT INTO schema_migrations VALUES(145,'0145_run_hold_spans.sql','sha256:5487f45980e844cc05e3a907aa97d069f4635992b16d0b831d7f9aadcc995fbd','2026-11-16T19:44:16.486244Z');
INSERT INTO schema_migrations VALUES(146,'0146_project_readiness.sql','sha256:ecdee123b8bde6d69bb03467cd4703ad2c42d1d18a252494b65463f96b963945','2026-11-16T19:44:16.48712Z');
CREATE TABLE server_state (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    sync_epoch TEXT    NOT NULL,
    revision   INTEGER NOT NULL
) STRICT;
INSERT INTO server_state VALUES(1,'033d458f644b70ce320d6af5726312a3',18);
CREATE TABLE runs (
    id             TEXT PRIMARY KEY,
    project_id     TEXT    NOT NULL,
    policy_digest  TEXT    NOT NULL,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT    NOT NULL
, campaign_id TEXT, attempt_number INTEGER, attempt_reason TEXT, parent_run_id TEXT REFERENCES runs (id), task_id TEXT NOT NULL DEFAULT '') STRICT;
INSERT INTO runs VALUES('run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','project-submit-elaboration','sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9',4,18,'{"id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","task_id":"task-WQR76VQI2IQ57ZI7ERIMD6BDHO","project_id":"project-submit-elaboration","spec_digest":"sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","policy_digest":"sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","campaign_id":"campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba","attempt_number":1,"base":null,"stages":[{"id":"elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","run_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","name":"specification","attempts":[{"id":"attempt-inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","stage_id":"elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","number":1,"invocation_id":"inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","purpose":null},{"id":"attempt-elaboration-discussion-explain-submitted-spec","stage_id":"elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","number":2,"invocation_id":"elaboration-discussion-explain-submitted-spec","purpose":null}]}]}','campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba',1,NULL,NULL,'task-WQR76VQI2IQ57ZI7ERIMD6BDHO');
INSERT INTO runs VALUES('implementation-from-submit','project-submit-elaboration','sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9',2,18,'{"id":"implementation-from-submit","task_id":"task-WQR76VQI2IQ57ZI7ERIMD6BDHO","project_id":"project-submit-elaboration","spec_digest":"sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","policy_digest":"sha256:5d991d9605ff1b7e97fbbcfde2392b04aa9f38e6b438ff559e2baa0c4ef9ffd9","campaign_id":"campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba","attempt_number":1,"base":null,"stages":[{"id":"implement-implementation-from-submit","run_id":"implementation-from-submit","name":"implement","attempts":[]}]}','campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba',1,NULL,NULL,'task-WQR76VQI2IQ57ZI7ERIMD6BDHO');
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
CHECK (readiness_detail IS NULL OR readiness_detail <> ''), subject_task_id TEXT REFERENCES tasks(id)) STRICT;
INSERT INTO attention_items VALUES('spec-approval-implementation-from-submit-1','project-submit-elaboration','conv-spec-approval-implementation-from-submit-1',4,18,'{"id":"spec-approval-implementation-from-submit-1","project_id":"project-submit-elaboration","subject":{"subject_type":"run","subject_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","run_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","task_id":"task-WQR76VQI2IQ57ZI7ERIMD6BDHO"},"type":"spec_approval","priority":"normal","reason":"Ready for implementation.","requested_decision":["approve","request_changes","discuss","stop"],"recommendation":null,"decision_surface":{"epoch":1,"digest":"sha256:1da37365a32af17b5fd4eebcb60ee5de5b318f93b5f11e53acec679bb10d6b94"},"evidence_snapshot":null,"agent_claims":[{"label":"Specification","artifact_id":"spec-implementation-from-submit-1","digest":"sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","provenance":{"producer_class":"agent","producer_invocation_id":"inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","head_binding":"head_independent","verification_recipe_digest":null,"sensitivity_class":"normal"},"text":{"media_type":"text/markdown","content":"# Approved specification\n\nImplement the thing within the declared paths."},"metadata":{"media_type":"text/markdown","size_bytes":72,"created_at":"2026-09-02T04:05:16Z","source":"claim","availability":"available"}},{"label":"freeside.summary","artifact_id":"spec-summary-implementation-from-submit-1","digest":"sha256:8e714f53a1974d5baffab99aff7e83fcd8f96d5ffc36c13b73fea0bed9b16dfe","provenance":{"producer_class":"agent","producer_invocation_id":"inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1","head_binding":"head_independent","verification_recipe_digest":null,"sensitivity_class":"normal"},"text":{"media_type":"text/markdown","content":"Ready for implementation."},"metadata":{"media_type":"text/markdown","size_bytes":25,"created_at":"2026-09-02T04:05:16Z","source":"claim","availability":"available"}}],"artifact_digests":["sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","sha256:8e714f53a1974d5baffab99aff7e83fcd8f96d5ffc36c13b73fea0bed9b16dfe"],"pr_head_sha":"","pr_reference":null,"readiness":null,"readiness_detail":null,"yield_history":null,"commit_plan_notice":null,"scope_decision":null,"readiness_invalidation":null,"review_recovery_binding":null,"codex_reenrollment_recovery_binding":null,"review_configuration_recovery":null,"finding_adjudication":null,"display_names":{"project":{"text":"project-submit-elaboration","source":"identifier"},"task":{"text":"task-WQR76VQI2IQ57ZI7ERIMD6BDHO","source":"identifier"}},"billable_cost_so_far":null,"execution_failure":null,"interpretation_failure":null,"publish_block":null,"diff_stats":null,"blocked_on":null,"health_diagnostic":null,"review_dispute":null,"spec_revision":null,"agent_question":null,"review_diminishing":null,"review_guide":null,"review_guide_notice":null,"alternate_agents":null,"elapsed_deadlines":null,"item_version":3,"interruption_class":"planned_gate","conversation_id":"conv-spec-approval-implementation-from-submit-1","timing":{"delivery_count":0,"first_submitted_at":null,"first_accepted_at":null,"first_opened_at":null,"submit_to_first_open":null},"created_at":"2026-09-02T04:05:16Z","expires_when":null,"decided_at":"2026-09-02T04:05:16Z","surface_first_opened_at":null,"conclusion":null,"posture":null,"blocking_supersession":null,"status":"resolved"}','spec_approval','resolved',NULL,'run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160',NULL,NULL,NULL,'task-WQR76VQI2IQ57ZI7ERIMD6BDHO');
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
CREATE TABLE inbox (
    id              INTEGER PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    kind            TEXT NOT NULL CHECK (kind <> ''),
    payload         BLOB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    created_at      TEXT NOT NULL
) STRICT;
INSERT INTO inbox VALUES(1,'inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1','elaboration_stage_terminal',x'7b22696e766f636174696f6e5f6964223a22696e762d656c61626f726174652d72756e2d656c61626f726174696f6e2d363934313933333339633535343562626561626563313962366663343631383236323564623736363933383131626134333565623138646362323630313136302d31222c22697465726174696f6e223a312c22737461747573223a22636f6d706c65746564222c2272657365617263685f61727469666163745f696473223a5b5d2c22737065635f61727469666163745f6964223a22737065632d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c22617070726f76616c5f6974656d5f6964223a22737065632d617070726f76616c2d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c2273756d6d6172795f646967657374223a227368613235363a38653731346635336131393734643562616666616239396166663765383366636438663936643566666333366331336237336665613062656439623136646665227d','pending','2026-09-02T04:05:17.033724Z');
INSERT INTO inbox VALUES(2,'inv-explain-submitted-spec','agent_completion',x'7b22696e766f636174696f6e5f6964223a22696e762d6578706c61696e2d7375626d69747465642d73706563222c22626f6479223a224974206b656570732074686520776f726b666c6f7720626f756e6465642062792070696e6e696e672074686520617070726f76656420617274696661637420616e64206465636c617265642073636f70652e222c226174746163686d656e7473223a6e756c6c7d','pending','2026-09-02T04:05:17.171037Z');
INSERT INTO inbox VALUES(3,'elaboration-discussion-explain-submitted-spec','elaboration_discussion_terminal',x'7b22696e766f636174696f6e5f6964223a22656c61626f726174696f6e2d64697363757373696f6e2d6578706c61696e2d7375626d69747465642d73706563222c22646973637573735f696e766f636174696f6e5f6964223a22696e762d6578706c61696e2d7375626d69747465642d73706563222c227265706c79223a224974206b656570732074686520776f726b666c6f7720626f756e6465642062792070696e6e696e672074686520617070726f76656420617274696661637420616e64206465636c617265642073636f70652e222c2264656c697665726564223a747275657d','pending','2026-09-02T04:05:17.173046Z');
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
    CHECK (repository_id >= 0), requested_issues TEXT NOT NULL DEFAULT '', granted_issues TEXT NOT NULL DEFAULT '', requested_workflows TEXT NOT NULL DEFAULT '', granted_workflows TEXT NOT NULL DEFAULT '') STRICT;
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
, audited_ref TEXT CHECK (audited_ref <> '')) STRICT;
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
    refresh_strategy          TEXT    NOT NULL CHECK (refresh_strategy <> ''),
    supports_read_only_auth_snapshot INTEGER NOT NULL
        CHECK (supports_read_only_auth_snapshot IN (0, 1)),
    recorded_at               TEXT NOT NULL CHECK (recorded_at <> ''),
    body                      TEXT NOT NULL
, auth_store_volume TEXT, account_binding TEXT NOT NULL DEFAULT '', usage_pool TEXT NOT NULL DEFAULT '', budget INTEGER NOT NULL DEFAULT 0 CHECK (budget >= 0), enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)), cost_owner TEXT NOT NULL DEFAULT '') STRICT;
INSERT INTO auth_identities VALUES('auth-submit','codex',1,'refresh_on_demand',0,'2026-09-02T04:05:16Z','{"identity":{"id":"auth-submit","provider":"codex","account_binding":"","usage_pool":"","budget":0,"auth_store_mutation_lease":true,"enabled":false,"cost_owner":"","interim":{"auth_store_volume":"provider-credentials","refresh_strategy":"refresh_on_demand","supports_read_only_auth_snapshot":false}},"recorded_at":"2026-09-02T04:05:16Z"}','provider-credentials','','',0,0,'');
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
, environment_digest TEXT
    CHECK (environment_digest IS NULL OR environment_digest <> '')) STRICT;
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
, cause TEXT NOT NULL DEFAULT '') STRICT;
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
, detail_check TEXT
    CHECK (detail_check IS NULL OR detail_check <> ''), detail_agent TEXT
    CHECK (detail_agent IS NULL OR detail_agent <> ''), detail_launch_stage TEXT
    CHECK (detail_launch_stage IS NULL OR detail_launch_stage <> ''), detail_launch_digest TEXT
    CHECK (detail_launch_digest IS NULL OR detail_launch_digest <> ''), upkeep_pending TEXT
    CHECK (upkeep_pending IS NULL OR upkeep_pending <> ''), upkeep_refusal_id TEXT
    CHECK (upkeep_refusal_id IS NULL OR upkeep_refusal_id <> ''), upkeep_item_id TEXT
    CHECK (upkeep_item_id IS NULL OR upkeep_item_id <> ''), upkeep_task_id TEXT
    CHECK (upkeep_task_id IS NULL OR upkeep_task_id <> ''), usage_pool TEXT
    CHECK (usage_pool IS NULL OR usage_pool <> ''), quota_pool TEXT
    CHECK (quota_pool IS NULL OR quota_pool <> ''), quota_window TEXT
    CHECK (quota_window IS NULL OR quota_window <> ''), quota_source TEXT
    CHECK (quota_source IS NULL OR quota_source <> ''), quota_invocation_id TEXT
    CHECK (quota_invocation_id IS NULL OR quota_invocation_id <> ''), quota_reading_observed_at TEXT
    CHECK (quota_reading_observed_at IS NULL OR quota_reading_observed_at <> ''), quota_resets_at TEXT
    CHECK (quota_resets_at IS NULL OR quota_resets_at <> ''), quota_set_at TEXT
    CHECK (quota_set_at IS NULL OR quota_set_at <> ''), capacity_reservation_mib INTEGER
    CHECK (capacity_reservation_mib IS NULL OR capacity_reservation_mib > 0), capacity_reserved_mib INTEGER
    CHECK (capacity_reserved_mib IS NULL OR capacity_reserved_mib >= 0), capacity_budget_mib INTEGER
    CHECK (capacity_budget_mib IS NULL OR capacity_budget_mib > 0), capacity_ahead INTEGER
    CHECK (capacity_ahead IS NULL OR capacity_ahead >= 0), quota_alternate_agents TEXT
    CHECK (quota_alternate_agents IS NULL OR quota_alternate_agents <> ''), detail_unserved_part TEXT
    CHECK (detail_unserved_part IS NULL OR detail_unserved_part <> ''), detail_unserved_enrollment TEXT
    CHECK (detail_unserved_enrollment IS NULL OR detail_unserved_enrollment <> '')) STRICT;
INSERT INTO run_hold_observations VALUES('implementation-from-submit','inv-implement-implementation-from-submit','attended_mode_active','2026-09-02T04:05:17.141511Z','2026-09-02T04:05:17.141511Z',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL);
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
CREATE TABLE effect_proposal_revisions (
    instance_id       TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    content_digest    TEXT NOT NULL CHECK (content_digest <> ''),
    supersedes_digest TEXT NOT NULL CHECK (supersedes_digest <> ''),
    command_id        TEXT NOT NULL UNIQUE REFERENCES commands (command_id),
    created_at        TEXT NOT NULL CHECK (created_at <> ''),
    body              TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (instance_id, content_digest)
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
    specification_run_id   TEXT    NOT NULL,
    implementation_run_id TEXT  NOT NULL UNIQUE,
    reason               TEXT    NOT NULL,
    as_of_revision       INTEGER NOT NULL,
    body                 TEXT    NOT NULL, publication_digest TEXT, revises_run_id TEXT,
    PRIMARY KEY (campaign_id, attempt_number)
) STRICT;
INSERT INTO production_attempts VALUES('campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba',1,'initial',NULL,'sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403','sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2','run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160','implementation-from-submit','',2,'{"campaign_id":"campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba","attempt_number":1,"kind":"initial","reason":"","source_digest":"sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","publication_digest":"sha256:f3c794222955e37bcacd199d0da87ea4f0cb721edd5a2d57b67ce9d86402219a","approved_spec_digest":"sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","elaboration_run_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","implementation_run_id":"implementation-from-submit"}','sha256:f3c794222955e37bcacd199d0da87ea4f0cb721edd5a2d57b67ce9d86402219a',NULL);
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
INSERT INTO attention_decision_surfaces VALUES('spec-approval-implementation-from-submit-1',1,'sha256:1da37365a32af17b5fd4eebcb60ee5de5b318f93b5f11e53acec679bb10d6b94','{"item_id":"spec-approval-implementation-from-submit-1","epoch":1,"subject":{"subject_type":"run","subject_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","run_id":"run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160","task_id":"task-WQR76VQI2IQ57ZI7ERIMD6BDHO"},"requested_decision":["approve","discuss","request_changes","stop"],"pr_head_sha":"","presented_artifact_digests":["sha256:7f0b8106f7545f8ce2d64d82f68d25772e1dc0c17d08bde2626fe5f5a4f239d2","sha256:8e714f53a1974d5baffab99aff7e83fcd8f96d5ffc36c13b73fea0bed9b16dfe"],"digest":"sha256:1da37365a32af17b5fd4eebcb60ee5de5b318f93b5f11e53acec679bb10d6b94"}');
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
CREATE TABLE device_capability_contracts (
    device_id     TEXT PRIMARY KEY REFERENCES devices (id),
    digest        TEXT NOT NULL CHECK (digest <> ''),
    body          TEXT NOT NULL,
    registered_at TEXT NOT NULL CHECK (registered_at <> '')
) STRICT;
CREATE TABLE decision_action_surfaces (
    digest                       TEXT PRIMARY KEY,
    device_id                    TEXT NOT NULL REFERENCES devices (id),
    item_id                      TEXT NOT NULL REFERENCES attention_items (id),
    item_decision_surface_digest TEXT NOT NULL CHECK (item_decision_surface_digest <> ''),
    client_capability_digest     TEXT NOT NULL CHECK (client_capability_digest <> ''),
    body                         TEXT NOT NULL,
    derived_at                   TEXT NOT NULL CHECK (derived_at <> '')
) STRICT;
CREATE TABLE comprehension_events (
    device_id                      TEXT    NOT NULL REFERENCES devices (id),
    event_id                       TEXT    NOT NULL,
    item_id                        TEXT    NOT NULL REFERENCES attention_items (id),
    kind                           TEXT    NOT NULL CHECK (kind <> ''),
    item_decision_surface_digest   TEXT    NOT NULL CHECK (item_decision_surface_digest <> ''),
    decision_action_surface_digest TEXT,
    command_id                     TEXT,
    occurred_at                    TEXT    NOT NULL CHECK (occurred_at <> ''),
    sequence                       INTEGER NOT NULL CHECK (sequence > 0),
    received_at                    TEXT    NOT NULL CHECK (received_at <> ''),
    body                           TEXT    NOT NULL,
    PRIMARY KEY (device_id, event_id)
) STRICT;
CREATE TABLE comprehension_defects (
    item_id      TEXT NOT NULL REFERENCES attention_items (id),
    claim_digest TEXT NOT NULL CHECK (claim_digest <> ''),
    recorded_at  TEXT NOT NULL CHECK (recorded_at <> ''),
    reason       TEXT NOT NULL CHECK (reason <> ''),
    PRIMARY KEY (item_id, claim_digest, recorded_at)
) STRICT;
CREATE TABLE run_milestones (
    id            INTEGER PRIMARY KEY,
    run_id        TEXT NOT NULL CHECK (run_id <> ''),
    kind          TEXT NOT NULL CHECK (kind IN (
                      'run_submitted', 'invocation_admitted',
                      'invocation_started', 'execution_export_recorded',
                      'execution_outcome_recorded', 'terminal_recorded',
                      'publication_ready', 'publication_blocked',
                      'work_unit_completed')),
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
CREATE TABLE review_requests (
    invocation_id TEXT PRIMARY KEY CHECK (invocation_id <> ''),
    run_id TEXT NOT NULL REFERENCES runs(id),
    round INTEGER NOT NULL CHECK (round > 0),
    base_sha TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha TEXT NOT NULL CHECK (head_sha <> ''),
    requested_at TEXT NOT NULL,
    body_digest TEXT NOT NULL,
    body TEXT NOT NULL,
    UNIQUE (run_id, round)
);
CREATE TABLE IF NOT EXISTS "outbox" (
    id INTEGER PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    kind TEXT NOT NULL CHECK (kind <> ''),
    payload BLOB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at TEXT NOT NULL,
    payload_version INTEGER NOT NULL DEFAULT 1 CHECK (payload_version IN (1, 2, 3, 4)),
    payload_digest TEXT NOT NULL DEFAULT ''
) STRICT;
INSERT INTO outbox VALUES(1,'inv-elaborate-run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160-1','elaboration_invocation_requested',x'7b2276657273696f6e223a2266726565736964652e656c61626f726174696f6e2d726571756573742f7631222c22656c61626f726174696f6e5f72756e5f6964223a2272756e2d656c61626f726174696f6e2d36393431393333333963353534356262656162656331396236666334363138323632356462373636393338313162613433356562313864636232363031313630222c22696d706c656d656e746174696f6e5f72756e5f6964223a22696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2270726f6a6563745f6964223a2270726f6a6563742d7375626d69742d656c61626f726174696f6e222c22696e766f636174696f6e5f6964223a22696e762d656c61626f726174652d72756e2d656c61626f726174696f6e2d363934313933333339633535343562626561626563313962366663343631383236323564623736363933383131626134333565623138646362323630313136302d31222c22697465726174696f6e223a312c22696e7075745f61727469666163745f696473223a5b2261727469666163742d73706563696669636174696f6e2d66366163396365353434646638376538303933393533303438356630326566373938366134643066303837656262646437643066663531653165626563343033225d2c22666565646261636b5f61727469666163745f696473223a6e756c6c2c22706f6c6963795f61727469666163745f6964223a2261727469666163742d706f6c6963792d35643939316439363035666631623765393766626263666465323339326230346161396633386536623433386666353539653262616130633465663966666439222c227075626c69636174696f6e223a7b227469746c65223a22546573742074686520776f726b206974656d222c22626f6479223a222323205768795c6e5c6e436c6f73657320233132332e5c6e222c22636f6d6d69745f617574686f72223a7b226170705f736c7567223a2266726565736964652d74657374222c22626f745f757365725f6964223a31323334357d7d2c227075626c69636174696f6e5f646967657374223a227368613235363a66336337393432323239353565333762636163643139396430646138376561346630636237323165646435613264353762363763653964383634303232313961222c2263616d706169676e5f6964223a2263616d706169676e2d62313363363438653537323564613231386232313338313836346535373637353066393364353431613235366261336562376135343466343738303039396261222c22617474656d70745f6e756d626572223a317d','dispatched','2026-09-02T04:05:16.928715Z',1,'sha256:7fd7eb3d3f8930e22cd632671957b949780800daa466b91a6883cfb646d22e2f');
INSERT INTO outbox VALUES(2,'claim-elaboration-implementation-implementation-from-submit','elaboration_implementation_claim',x'7b2276657273696f6e223a2266726565736964652e656c61626f726174696f6e2d726571756573742f7631222c22656c61626f726174696f6e5f72756e5f6964223a2272756e2d656c61626f726174696f6e2d36393431393333333963353534356262656162656331396236666334363138323632356462373636393338313162613433356562313864636232363031313630222c22696d706c656d656e746174696f6e5f72756e5f6964223a22696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2270726f6a6563745f6964223a2270726f6a6563742d7375626d69742d656c61626f726174696f6e222c22696e766f636174696f6e5f6964223a22696e762d656c61626f726174652d72756e2d656c61626f726174696f6e2d363934313933333339633535343562626561626563313962366663343631383236323564623736363933383131626134333565623138646362323630313136302d31222c22697465726174696f6e223a312c22696e7075745f61727469666163745f696473223a5b2261727469666163742d73706563696669636174696f6e2d66366163396365353434646638376538303933393533303438356630326566373938366134643066303837656262646437643066663531653165626563343033225d2c22666565646261636b5f61727469666163745f696473223a6e756c6c2c22706f6c6963795f61727469666163745f6964223a2261727469666163742d706f6c6963792d35643939316439363035666631623765393766626263666465323339326230346161396633386536623433386666353539653262616130633465663966666439222c227075626c69636174696f6e223a7b227469746c65223a22546573742074686520776f726b206974656d222c22626f6479223a222323205768795c6e5c6e436c6f73657320233132332e5c6e222c22636f6d6d69745f617574686f72223a7b226170705f736c7567223a2266726565736964652d74657374222c22626f745f757365725f6964223a31323334357d7d2c227075626c69636174696f6e5f646967657374223a227368613235363a66336337393432323239353565333762636163643139396430646138376561346630636237323165646435613264353762363763653964383634303232313961222c2263616d706169676e5f6964223a2263616d706169676e2d62313363363438653537323564613231386232313338313836346535373637353066393364353431613235366261336562376135343466343738303039396261222c22617474656d70745f6e756d626572223a317d','dispatched','2026-09-02T04:05:16.928769Z',1,'sha256:7fd7eb3d3f8930e22cd632671957b949780800daa466b91a6883cfb646d22e2f');
INSERT INTO outbox VALUES(3,'inv-explain-submitted-spec','agent_invocation_requested',x'7b22696e766f636174696f6e5f6964223a22696e762d6578706c61696e2d7375626d69747465642d73706563222c22636f6e766572736174696f6e5f6964223a22636f6e762d737065632d617070726f76616c2d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c226974656d5f6964223a22737065632d617070726f76616c2d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c226974656d5f76657273696f6e223a327d','dispatched','2026-09-02T04:05:17.053855Z',1,'sha256:8a88af760a1509aa3e0f82ba0d6b9ee6d0316e57061c1eae023f17b9586facfe');
INSERT INTO outbox VALUES(4,'elaboration-discussion-explain-submitted-spec','elaboration_discussion_requested',x'7b2276657273696f6e223a2266726565736964652e656c61626f726174696f6e2d64697363757373696f6e2d726571756573742f7631222c22656c61626f726174696f6e5f72756e5f6964223a2272756e2d656c61626f726174696f6e2d36393431393333333963353534356262656162656331396236666334363138323632356462373636393338313162613433356562313864636232363031313630222c22696d706c656d656e746174696f6e5f72756e5f6964223a22696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2270726f6a6563745f6964223a2270726f6a6563742d7375626d69742d656c61626f726174696f6e222c22697465726174696f6e223a312c22696e766f636174696f6e5f6964223a22656c61626f726174696f6e2d64697363757373696f6e2d6578706c61696e2d7375626d69747465642d73706563222c22646973637573735f696e766f636174696f6e5f6964223a22696e762d6578706c61696e2d7375626d69747465642d73706563222c22636f6e766572736174696f6e5f6964223a22636f6e762d737065632d617070726f76616c2d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c227468726f7567685f73657175656e6365223a312c227072656669785f646967657374223a227368613235363a39373430653739393339333065386266353934646565383662626437356237626235376233393562313233366365336562313639373065643461366437346338222c226974656d5f6964223a22737065632d617070726f76616c2d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c226974656d5f76657273696f6e223a322c22696e7075745f61727469666163745f696473223a5b2261727469666163742d73706563696669636174696f6e2d66366163396365353434646638376538303933393533303438356630326566373938366134643066303837656262646437643066663531653165626563343033222c22737065632d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c22737065632d64697363757373696f6e2d6578706c61696e2d7375626d69747465642d73706563225d2c22737065635f61727469666163745f6964223a22737065632d696d706c656d656e746174696f6e2d66726f6d2d7375626d69742d31222c22706f6c6963795f61727469666163745f6964223a2261727469666163742d706f6c6963792d35643939316439363035666631623765393766626263666465323339326230346161396633386536623433386666353539653262616130633465663966666439227d','dispatched','2026-09-02T04:05:17.073832Z',1,'sha256:1d5564eee1013e3aeda16951abd9e855ee6a8f62a9b46037a04527e78f8b76b6');
INSERT INTO outbox VALUES(5,'inv-implement-implementation-from-submit','production_invocation_requested',x'7b2276657273696f6e223a2266726565736964652e70726f64756374696f6e2d696e766f636174696f6e2f7632222c22696e766f636174696f6e5f6964223a22696e762d696d706c656d656e742d696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2272756e5f6964223a22696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2273746167655f6964223a22696d706c656d656e742d696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c227075626c69636174696f6e223a7b227469746c65223a22546573742074686520776f726b206974656d222c22626f6479223a222323205768795c6e5c6e436c6f73657320233132332e5c6e222c22636f6d6d69745f617574686f72223a7b226170705f736c7567223a2266726565736964652d74657374222c22626f745f757365725f6964223a31323334357d7d7d','pending','2026-09-02T04:05:17.078299Z',1,'sha256:95ffd18543d936497890632da872db2d733035a5dc10036d9f42bc5ee09f5d2e');
INSERT INTO outbox VALUES(6,'publish/publish-production-implementation-from-submit/publish.publication','publish.invocation_reservation',x'7b2276657273696f6e223a2266726565736964652d7075626c69636174696f6e2d7265736572766174696f6e2f7631222c22696e766f636174696f6e5f6964223a227075626c6973682d70726f64756374696f6e2d696d706c656d656e746174696f6e2d66726f6d2d7375626d6974222c2272756e5f6964223a22696d706c656d656e746174696f6e2d66726f6d2d7375626d6974227d','pending','2026-09-02T04:05:17.078408Z',1,'sha256:81c389f9b118ac5d2ce09e973a9f1298913034e16677e4679e6bf2a510fafe3c');
CREATE TABLE IF NOT EXISTS "ready_item_pr_bindings" (
    item_id TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    run_id TEXT NOT NULL REFERENCES runs (id),
    producing_invocation_id TEXT NOT NULL REFERENCES execution_admissions (invocation_id),
    publication_invocation_id TEXT NOT NULL UNIQUE,
    publication_identity TEXT NOT NULL CHECK (publication_identity <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number INTEGER NOT NULL CHECK (pr_number > 0),
    body TEXT NOT NULL CHECK (body <> ''),
    recorded_at TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;
CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    entity_version INTEGER NOT NULL CHECK (entity_version >= 1),
    as_of_revision INTEGER NOT NULL CHECK (as_of_revision >= 1),
    body TEXT NOT NULL
) STRICT;
INSERT INTO tasks VALUES('task-WQR76VQI2IQ57ZI7ERIMD6BDHO','project-submit-elaboration',3,18,'{"id":"task-WQR76VQI2IQ57ZI7ERIMD6BDHO","project_id":"project-submit-elaboration","source":{"kind":"work_item_artifact","work_item_artifact_id":"artifact-specification-f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403","issue_subject":null},"name":{"text":"task-WQR76VQI2IQ57ZI7ERIMD6BDHO","source":"identifier"},"created_at":"2026-09-02T04:05:16.928823Z","campaign_ids":["campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba"],"lifecycle_facts":null,"cancellation":null}');
CREATE TABLE task_intake_keys (
    project_id TEXT NOT NULL,
    intake_key TEXT NOT NULL,
    task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) DEFERRABLE INITIALLY DEFERRED,
    PRIMARY KEY (project_id, intake_key)
) STRICT;
INSERT INTO task_intake_keys VALUES('project-submit-elaboration','source:sha256:f6ac9ce544df87e80939530485f02ef7986a4d0f087ebbdd7d0ff51e1ebec403','task-WQR76VQI2IQ57ZI7ERIMD6BDHO');
CREATE TABLE task_runs (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    run_id TEXT NOT NULL UNIQUE REFERENCES runs(id),
    PRIMARY KEY (task_id, ordinal)
) STRICT;
INSERT INTO task_runs VALUES('task-WQR76VQI2IQ57ZI7ERIMD6BDHO',1,'run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160');
INSERT INTO task_runs VALUES('task-WQR76VQI2IQ57ZI7ERIMD6BDHO',2,'implementation-from-submit');
CREATE TABLE task_lifecycle_facts (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    kind TEXT NOT NULL CHECK (kind IN ('started', 'completed', 'abandoned')),
    run_id TEXT NOT NULL,
    campaign_id TEXT,
    binding_unit_id TEXT,
    source_id TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (task_id, ordinal),
    UNIQUE (task_id, source_id)
) STRICT;
INSERT INTO task_lifecycle_facts VALUES('task-WQR76VQI2IQ57ZI7ERIMD6BDHO',1,'started','implementation-from-submit','campaign-b13c648e5725da218b21381864e576750f93d541a256ba3eb7a544f4780099ba',NULL,'migration:start','2026-11-16T19:44:16.144678Z');
CREATE TABLE manual_submissions (
    identity TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    source_artifact_id TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    implementation_run_id TEXT NOT NULL UNIQUE
, task_lines TEXT) STRICT;
CREATE TABLE task_cancellations (
    request_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    target_digest TEXT NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body TEXT NOT NULL,
    UNIQUE(task_id, target_digest)
) STRICT;
CREATE TABLE task_stop_commands (
    command_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL REFERENCES task_cancellations(request_id),
    as_of_revision INTEGER NOT NULL,
    body TEXT NOT NULL
) STRICT;
CREATE TABLE task_cancellation_acknowledgements (
    id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL REFERENCES task_cancellations(request_id),
    as_of_revision INTEGER NOT NULL,
    body TEXT NOT NULL
) STRICT;
CREATE TABLE publication_authorings (
    content_digest TEXT NOT NULL CHECK (content_digest <> ''),
    run_id         TEXT NOT NULL REFERENCES runs (id),
    created_at     TEXT NOT NULL,
    body_digest    TEXT NOT NULL,
    body           TEXT NOT NULL,
    PRIMARY KEY (content_digest)
) STRICT;
CREATE TABLE effect_proposal_decisions (
    instance_id     TEXT NOT NULL PRIMARY KEY REFERENCES effect_proposal_instances (instance_id),
    command_id      TEXT NOT NULL UNIQUE REFERENCES commands (command_id),
    action          TEXT NOT NULL CHECK (action IN
        ('start', 'start_with_changes', 'approve', 'approve_with_changes', 'decline')),
    selected_digest TEXT,
    decided_at      TEXT NOT NULL CHECK (decided_at <> ''),
    CHECK ((action IN ('start', 'start_with_changes', 'approve', 'approve_with_changes')
            AND selected_digest IS NOT NULL AND selected_digest <> '')
        OR (action = 'decline' AND selected_digest IS NULL))
) STRICT;
CREATE TABLE effect_proposal_items (
    item_id              TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    instance_id          TEXT NOT NULL REFERENCES effect_proposal_instances (instance_id),
    content_digest       TEXT NOT NULL CHECK (content_digest <> ''),
    publication_identity TEXT,
    candidate_head_sha   TEXT,
    base_ref             TEXT,
    base_sha             TEXT,
    CHECK ((publication_identity IS NULL AND candidate_head_sha IS NULL
            AND base_ref IS NULL AND base_sha IS NULL)
        OR (publication_identity IS NOT NULL AND publication_identity <> ''
            AND candidate_head_sha IS NOT NULL AND candidate_head_sha <> ''
            AND base_ref IS NOT NULL AND base_ref <> ''
            AND base_sha IS NOT NULL AND base_sha <> ''))
) STRICT;
CREATE TABLE effect_proposal_policy_approvals (
    instance_id          TEXT NOT NULL PRIMARY KEY REFERENCES effect_proposal_instances (instance_id),
    proposal_digest      TEXT NOT NULL CHECK (proposal_digest <> ''),
    publication_identity TEXT NOT NULL CHECK (publication_identity <> ''),
    candidate_head_sha   TEXT NOT NULL CHECK (candidate_head_sha <> ''),
    base_ref             TEXT NOT NULL CHECK (base_ref <> ''),
    base_sha             TEXT NOT NULL CHECK (base_sha <> ''),
    approved_at          TEXT NOT NULL CHECK (approved_at <> '')
) STRICT;
CREATE TABLE auth_store_read_holds (
    auth_identity_id     TEXT    NOT NULL REFERENCES auth_identities (id),
    holder               TEXT    NOT NULL CHECK (holder <> ''),
    acquired_at          TEXT    NOT NULL CHECK (acquired_at <> ''),
    expires_at           TEXT    NOT NULL CHECK (expires_at <> ''),
    expires_at_unix_nano INTEGER NOT NULL,
    released_at          TEXT,
    body                 TEXT    NOT NULL,
    PRIMARY KEY (auth_identity_id, holder)
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
    read_hold_auth_identity_id TEXT REFERENCES auth_identities (id),
    read_hold_holder          TEXT,
    read_hold_acquired_at     TEXT,
    read_hold_expires_at      TEXT,
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
    ),
    CHECK (
        (read_hold_auth_identity_id IS NULL AND read_hold_holder IS NULL AND
         read_hold_acquired_at IS NULL AND read_hold_expires_at IS NULL)
        OR
        (read_hold_auth_identity_id IS NOT NULL AND read_hold_holder IS NOT NULL AND
         read_hold_acquired_at IS NOT NULL AND read_hold_expires_at IS NOT NULL)
    ),
    CHECK (lease_auth_identity_id IS NULL OR read_hold_auth_identity_id IS NULL)
) STRICT;
CREATE TABLE review_round_diff_metrics (
    run_id      TEXT    NOT NULL REFERENCES runs (id),
    round       INTEGER NOT NULL CHECK (round > 0),
    body_digest TEXT    NOT NULL CHECK (body_digest <> ''),
    body        TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (run_id, round),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round)
) STRICT;
CREATE TABLE drift_audits (
    run_id         TEXT    NOT NULL REFERENCES runs (id),
    round          INTEGER NOT NULL CHECK (round > 0),
    content_digest TEXT    NOT NULL CHECK (content_digest <> ''),
    body_digest    TEXT    NOT NULL CHECK (body_digest <> ''),
    body           TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (run_id, round),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round)
) STRICT;
CREATE TABLE finding_disposition_supersessions (
    finding_id       TEXT    NOT NULL,
    superseded_round INTEGER NOT NULL CHECK (superseded_round > 0),
    run_id           TEXT    NOT NULL REFERENCES runs (id),
    reversing_round  INTEGER NOT NULL,
    body_digest      TEXT    NOT NULL CHECK (body_digest <> ''),
    body             TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (finding_id, superseded_round),
    FOREIGN KEY (finding_id, superseded_round) REFERENCES finding_dispositions (finding_id, round),
    FOREIGN KEY (run_id, reversing_round) REFERENCES review_records (run_id, round),
    CHECK (reversing_round > superseded_round)
) STRICT;
CREATE TABLE held_item_pr_bindings (
    item_id TEXT NOT NULL PRIMARY KEY REFERENCES attention_items (id),
    run_id TEXT NOT NULL REFERENCES runs (id),
    producing_invocation_id TEXT NOT NULL REFERENCES execution_admissions (invocation_id),
    publication_invocation_id TEXT NOT NULL CHECK (publication_invocation_id <> ''),
    publication_identity TEXT NOT NULL CHECK (publication_identity <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number INTEGER NOT NULL CHECK (pr_number > 0),
    body TEXT NOT NULL CHECK (body <> ''),
    recorded_at TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;
CREATE TABLE generation_integrity_marks (
    enrollment_id TEXT    NOT NULL CHECK (enrollment_id <> ''),
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 1),
    finding       TEXT    NOT NULL CHECK (finding <> ''),
    observed_at   TEXT    NOT NULL CHECK (observed_at <> ''),
    body          TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (enrollment_id, ordinal, finding),
    FOREIGN KEY (enrollment_id, ordinal)
        REFERENCES client_enrollment_generations (enrollment_id, ordinal)
) STRICT;
CREATE TABLE follow_up_filing_intents (
    instance_id                TEXT    PRIMARY KEY
        REFERENCES effect_proposal_instances (instance_id),
    repository_id              INTEGER NOT NULL CHECK (repository_id > 0),
    opened_at                  TEXT    NOT NULL CHECK (opened_at <> ''),
    pre_dispatch_issue_numbers TEXT,
    pre_dispatch_recorded_at   TEXT,
    outcome                    TEXT,
    refusal_reason             TEXT,
    resolved_at                TEXT, pre_dispatch_bot_user_id INTEGER
    CHECK (pre_dispatch_bot_user_id IS NULL OR pre_dispatch_bot_user_id > 0),
    CHECK ((pre_dispatch_issue_numbers IS NULL) = (pre_dispatch_recorded_at IS NULL)),
    CHECK ((outcome IS NULL) = (resolved_at IS NULL)),
    CHECK ((refusal_reason IS NOT NULL) = (outcome IS NOT NULL AND outcome = 'refused'))
) STRICT;
CREATE TABLE follow_up_filing_attempts (
    instance_id           TEXT    NOT NULL
        REFERENCES follow_up_filing_intents (instance_id),
    ordinal               INTEGER NOT NULL CHECK (ordinal >= 1),
    dispatch_started_at   TEXT    NOT NULL CHECK (dispatch_started_at <> ''),
    response_class        TEXT,
    response_issue_number INTEGER CHECK (response_issue_number > 0),
    response_recorded_at  TEXT,
    PRIMARY KEY (instance_id, ordinal),
    CHECK ((response_class IS NULL) = (response_recorded_at IS NULL)),
    CHECK ((response_issue_number IS NOT NULL)
        = (response_class IS NOT NULL AND response_class = 'success'))
) STRICT;
CREATE TABLE follow_up_filed_issues (
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    issue_number  INTEGER NOT NULL CHECK (issue_number > 0),
    instance_id   TEXT    NOT NULL UNIQUE
        REFERENCES follow_up_filing_intents (instance_id),
    body          TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (repository_id, issue_number)
) STRICT;
CREATE TABLE external_finding_dispositions (
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
CREATE TABLE device_activity (
    device_id    TEXT PRIMARY KEY REFERENCES devices (id),
    last_seen_at INTEGER NOT NULL CHECK (last_seen_at > 0)
) STRICT;
CREATE TABLE review_admissions (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    role          TEXT NOT NULL CHECK (role IN ('reviewer', 'shadow_reviewer')),
    agent_digest  TEXT NOT NULL CHECK (agent_digest <> ''),
    admitted_at   TEXT NOT NULL CHECK (admitted_at <> ''),
    body_digest   TEXT NOT NULL CHECK (body_digest <> ''),
    body          TEXT NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE review_dispute_rows (
    item_id     TEXT PRIMARY KEY REFERENCES attention_items(id),
    finding_ids TEXT NOT NULL
) STRICT;
CREATE TABLE publish_installation_mint_holds (
    installation_id INTEGER PRIMARY KEY CHECK (installation_id > 0),
    registration_id INTEGER NOT NULL CHECK (registration_id > 0),
    requested_at    TEXT    NOT NULL CHECK (requested_at <> ''),
    held_until      TEXT    NOT NULL CHECK (held_until <> '')
) STRICT;
CREATE TABLE task_workflow_closures (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    start_ordinal INTEGER NOT NULL CHECK (start_ordinal > 0),
    start_run_id TEXT NOT NULL REFERENCES runs(id),
    revision INTEGER NOT NULL CHECK (revision > 0),
    body TEXT NOT NULL,
    PRIMARY KEY (task_id, start_ordinal),
    FOREIGN KEY (task_id, start_ordinal) REFERENCES task_lifecycle_facts(task_id, ordinal)
) STRICT;
CREATE TABLE task_attention_workflow_origins (
    item_id TEXT PRIMARY KEY REFERENCES attention_items(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    run_id TEXT NOT NULL REFERENCES runs(id),
    body TEXT NOT NULL
) STRICT;
CREATE TABLE work_unit_rebase_proofs (
    unit_id         TEXT NOT NULL REFERENCES work_unit_pr_bindings (unit_id),
    proven_head_sha TEXT NOT NULL CHECK (proven_head_sha <> ''),
    body            TEXT NOT NULL CHECK (body <> ''),
    recorded_at     TEXT NOT NULL CHECK (recorded_at <> ''),
    PRIMARY KEY (unit_id, proven_head_sha)
) STRICT;
CREATE TABLE usage_observations (
    invocation_id    TEXT    NOT NULL CHECK (invocation_id <> ''),
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
CREATE TABLE admitted_image_pointers (
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    harness       TEXT NOT NULL CHECK (harness <> ''),
    image_id      TEXT NOT NULL CHECK (image_id <> ''),
    image_ref     TEXT NOT NULL CHECK (image_ref <> ''),
    advanced_at   TEXT NOT NULL CHECK (advanced_at <> ''),
    body          TEXT NOT NULL,
    PRIMARY KEY (repository_id, harness)
) STRICT;
CREATE TABLE schedules (
    id             TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    project_id     TEXT NOT NULL CHECK (project_id <> ''),
    kind           TEXT NOT NULL CHECK (kind IN (
                       'pr_checks_deadline', 'review_wait_threshold',
                       'base_advance_watch', 'installation_poll',
                       'doctor', 'janitor', 'image_upkeep')),
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
CREATE TABLE image_upkeep_refusals (
    id            TEXT PRIMARY KEY CHECK (id <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    harness       TEXT NOT NULL CHECK (harness <> ''),
    tip_sha       TEXT NOT NULL CHECK (length(tip_sha) = 40),
    class         TEXT NOT NULL CHECK (class <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
) STRICT;
CREATE TABLE repository_tip_observations (
    repository_id  INTEGER PRIMARY KEY CHECK (repository_id > 0),
    branch         TEXT NOT NULL CHECK (branch <> ''),
    tip_sha        TEXT NOT NULL CHECK (length(tip_sha) = 40),
    observed_at    TEXT NOT NULL CHECK (observed_at <> ''),
    upkeep_tip_sha TEXT CHECK (upkeep_tip_sha IS NULL OR length(upkeep_tip_sha) = 40),
    body           TEXT NOT NULL
) STRICT;
CREATE TABLE task_submission_commands (
    command_id           TEXT PRIMARY KEY,
    origin               TEXT    NOT NULL CHECK (origin IN ('device', 'daemon')),
    device_id            TEXT,
    project_id           TEXT    NOT NULL,
    task_id              TEXT    NOT NULL REFERENCES tasks (id),
    specification_run_id TEXT    NOT NULL REFERENCES runs (id),
    entity_version       INTEGER NOT NULL,
    as_of_revision       INTEGER NOT NULL,
    body                 TEXT    NOT NULL,
    request_digest       TEXT,
    CHECK ((origin = 'device') = (device_id IS NOT NULL))
) STRICT;
CREATE TABLE image_policy_adoptions (
    id            TEXT PRIMARY KEY CHECK (id <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    instance_id   TEXT NOT NULL UNIQUE REFERENCES effect_proposal_instances (instance_id),
    refusal_id    TEXT NOT NULL REFERENCES image_upkeep_refusals (id),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
) STRICT;
CREATE TABLE usage_pools (
    id                      TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    provider                TEXT NOT NULL CHECK (provider <> ''),
    max_parallel_executions INTEGER NOT NULL CHECK (max_parallel_executions >= 1),
    recorded_at             TEXT NOT NULL CHECK (recorded_at <> ''),
    body                    TEXT NOT NULL CHECK (body <> '')
) STRICT;
INSERT INTO usage_pools VALUES('identity:auth-submit','codex',2,'2026-09-02T04:05:16Z','{"pool":{"id":"identity:auth-submit","provider":"codex","max_parallel_executions":2,"max_calls_per_site":1},"recorded_at":"2026-09-02T04:05:16Z"}');
CREATE TABLE task_line_changes (
    id          TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    task_id     TEXT NOT NULL REFERENCES tasks (id),
    role        TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    body        TEXT NOT NULL
) STRICT;
CREATE TABLE review_slot_releases (
    invocation_id TEXT NOT NULL PRIMARY KEY REFERENCES review_admissions (invocation_id),
    released_at   TEXT NOT NULL CHECK (released_at <> '')
) STRICT;
CREATE TABLE routing_policies (
    revision        INTEGER NOT NULL PRIMARY KEY CHECK (revision > 0),
    source          TEXT    NOT NULL CHECK (source <> ''),
    set_by          TEXT    NOT NULL CHECK (set_by <> ''),
    set_at          TEXT    NOT NULL CHECK (set_at <> ''),
    lineup_revision TEXT    NOT NULL CHECK (lineup_revision <> ''),
    body            TEXT    NOT NULL CHECK (body <> '')
, drain_first_account TEXT
    CHECK (drain_first_account IS NULL OR drain_first_account <> '')) STRICT;
CREATE TABLE pool_usage_observations (
    pool                TEXT NOT NULL PRIMARY KEY REFERENCES usage_pools (id),
    collector           TEXT NOT NULL REFERENCES client_enrollments (id),
    last_attempt_status TEXT CHECK (last_attempt_status IS NULL OR last_attempt_status <> ''),
    last_attempt_at     TEXT CHECK ((last_attempt_status IS NULL) = (last_attempt_at IS NULL)),
    recorded_at         TEXT NOT NULL CHECK (recorded_at <> ''),
    body                TEXT NOT NULL CHECK (body <> '')
, as_of_revision INTEGER NOT NULL DEFAULT 0, entity_version INTEGER NOT NULL DEFAULT 0) STRICT;
CREATE TABLE client_command_receipts (
    command_id     TEXT    NOT NULL PRIMARY KEY CHECK (command_id <> ''),
    kind           TEXT    NOT NULL CHECK (kind <> ''),
    as_of_revision INTEGER NOT NULL CHECK (as_of_revision > 0),
    body           TEXT    NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE memory_budget (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    budget_mib  INTEGER NOT NULL CHECK (budget_mib > 0),
    recorded_at TEXT    NOT NULL CHECK (recorded_at <> '')
) STRICT;
CREATE TABLE task_triages (
    task_id     TEXT    NOT NULL PRIMARY KEY REFERENCES tasks (id),
    outcome     TEXT    NOT NULL CHECK (outcome IN
        ('answered', 'call_failed', 'invalid_answer', 'budget_spent', 'no_triager_line')),
    tier        TEXT    CHECK (tier IS NULL OR tier IN ('standard', 'escalated')),
    reason      TEXT    CHECK (reason IS NULL OR reason <> ''),
    escalated   INTEGER NOT NULL CHECK (escalated IN (0, 1)),
    recorded_at TEXT    NOT NULL,
    CHECK ((outcome = 'answered') = (tier IS NOT NULL)),
    CHECK (escalated = (outcome <> 'answered' OR tier <> 'standard'))
) STRICT;
CREATE TABLE task_attachments (
    task_id            TEXT    NOT NULL REFERENCES tasks (id),
    position           INTEGER NOT NULL CHECK (position >= 0),
    digest             TEXT    NOT NULL CHECK (digest <> ''),
    name               TEXT    NOT NULL CHECK (name <> ''),
    media_type         TEXT    NOT NULL CHECK (media_type IN
        ('image/png', 'image/jpeg', 'image/gif', 'image/webp', 'application/octet-stream')),
    claimed_media_type TEXT    NOT NULL CHECK (claimed_media_type <> ''),
    size_bytes         INTEGER NOT NULL CHECK (size_bytes >= 0),
    PRIMARY KEY (task_id, position),
    UNIQUE (task_id, name)
) STRICT;
CREATE TABLE alternate_agent_retries (
    id          TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    task_id     TEXT NOT NULL REFERENCES tasks (id),
    command_id  TEXT NOT NULL UNIQUE CHECK (command_id <> ''),
    recorded_at TEXT NOT NULL,
    body        TEXT NOT NULL
) STRICT;
CREATE TABLE publication_handoff_checks (
    run_id                    TEXT NOT NULL REFERENCES runs (id),
    publication_id            TEXT NOT NULL CHECK (publication_id <> ''),
    head_sha                  TEXT NOT NULL CHECK (head_sha <> ''),
    evidence_base_sha         TEXT NOT NULL CHECK (evidence_base_sha <> ''),
    tip_sha                   TEXT NOT NULL CHECK (tip_sha <> ''),
    merge_sha                 TEXT CHECK (merge_sha <> ''),
    verification_proof_digest TEXT REFERENCES check_proofs (digest),
    resolved_at               TEXT NOT NULL,
    proved_at                 TEXT, workflow_audit_id INTEGER REFERENCES workflow_audits (id),
    CHECK ((merge_sha IS NULL) = (verification_proof_digest IS NULL)),
    CHECK ((merge_sha IS NULL) = (proved_at IS NULL)),
    CHECK (merge_sha IS NULL OR tip_sha <> evidence_base_sha),
    PRIMARY KEY (run_id, publication_id)
);
CREATE TABLE discovered_model_catalogs (
    enrollment_id TEXT NOT NULL PRIMARY KEY REFERENCES client_enrollments (id),
    harness_build TEXT NOT NULL CHECK (harness_build <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> ''),
    body          TEXT NOT NULL
) STRICT;
CREATE TABLE task_lines (
    task_id        TEXT    NOT NULL REFERENCES tasks (id),
    role           TEXT    NOT NULL CHECK (role IN ('specifier', 'implementer', 'remediator', 'reviewer')),
    version        INTEGER NOT NULL CHECK (version > 0),
    id             TEXT    NOT NULL UNIQUE CHECK (id <> ''),
    encoding       TEXT    NOT NULL CHECK (encoding IN ('freeside.task_line/v1', 'freeside.task_line/v2')),
    agent          TEXT    CHECK (agent <> ''),
    enrollment_id  TEXT    CHECK (enrollment_id <> ''),
    model_id       TEXT    CHECK (model_id <> ''),
    effort         TEXT    CHECK (effort <> ''),
    predecessor_id TEXT,
    source         TEXT    NOT NULL CHECK (source <> ''),
    set_by         TEXT    NOT NULL CHECK (set_by <> ''),
    set_at         TEXT    NOT NULL,
    provenance     TEXT    CHECK ((source IN ('router', 'alternate_agent_move')) = (provenance IS NOT NULL)),
    cleared        INTEGER NOT NULL DEFAULT 0 CHECK (cleared IN (0, 1)),
    CHECK ((version = 1) = (predecessor_id IS NULL)),
    CHECK ((enrollment_id IS NULL) = (model_id IS NULL) AND (model_id IS NULL) = (effort IS NULL)),
    CHECK (encoding = 'freeside.task_line/v2' OR enrollment_id IS NULL),
    CHECK (encoding = 'freeside.task_line/v1' OR agent IS NULL),
    CHECK ((cleared = 1) = (agent IS NULL AND enrollment_id IS NULL)),
    PRIMARY KEY (task_id, role, version)
) STRICT;
CREATE TABLE device_notification_preferences (
    device_id  TEXT    PRIMARY KEY REFERENCES devices (id),
    enabled    INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    updated_at TEXT    NOT NULL CHECK (updated_at <> '')
) STRICT;
CREATE TABLE attention_item_views (
    item_id                 TEXT    NOT NULL REFERENCES attention_items (id),
    decision_surface_digest TEXT    NOT NULL CHECK (decision_surface_digest <> ''),
    first_opened_at         TEXT    NOT NULL CHECK (first_opened_at <> ''),
    as_of_revision          INTEGER NOT NULL,
    PRIMARY KEY (item_id, decision_surface_digest)
) STRICT;
CREATE TABLE update_drain_transitions (
    id          INTEGER PRIMARY KEY,
    state       TEXT NOT NULL CHECK (state <> ''),
    build       TEXT NOT NULL CHECK (build <> ''),
    forced      INTEGER NOT NULL CHECK (forced IN (0, 1)),
    occurred_at TEXT NOT NULL CHECK (occurred_at <> '')
) STRICT;
CREATE TABLE duplicate_searches (
    run_id              TEXT NOT NULL REFERENCES runs (id),
    adjudication_digest TEXT NOT NULL CHECK (adjudication_digest <> ''),
    finding_id          TEXT NOT NULL CHECK (finding_id <> ''),
    content_digest      TEXT NOT NULL CHECK (content_digest <> ''),
    body_digest         TEXT NOT NULL CHECK (body_digest <> ''),
    body                TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (adjudication_digest, finding_id)
) STRICT;
CREATE TABLE workflow_policy_revisions (
    revision   INTEGER NOT NULL PRIMARY KEY CHECK (revision > 0),
    scope      TEXT    NOT NULL CHECK (scope <> ''),
    project_id TEXT    NOT NULL,
    key        TEXT    NOT NULL CHECK (key <> ''),
    set_by     TEXT    NOT NULL CHECK (set_by <> ''),
    set_at     TEXT    NOT NULL CHECK (set_at <> ''),
    body       TEXT    NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE effect_proposal_instances (
    instance_id             TEXT NOT NULL PRIMARY KEY CHECK (instance_id <> ''),
    admission_key           TEXT NOT NULL UNIQUE CHECK (admission_key <> ''),
    proposal_batch_id       TEXT NOT NULL CHECK (proposal_batch_id <> ''),
    effect_kind             TEXT NOT NULL CHECK (effect_kind IN
        ('run_proposal', 'source_issue_closure', 'follow_up_filing', 'image_policy_adoption',
         'issue_comment')),
    content_digest          TEXT NOT NULL CHECK (content_digest <> ''),
    resolved_policy_run_id  TEXT REFERENCES resolved_policies (run_id),
    resolved_policy_digest  TEXT,
    subject_handle          TEXT NOT NULL CHECK (subject_handle <> ''),
    created_at              TEXT NOT NULL CHECK (created_at <> ''),
    body                    TEXT NOT NULL CHECK (body <> ''),
    CHECK (CASE effect_kind
        WHEN 'image_policy_adoption'
            THEN resolved_policy_run_id IS NULL AND resolved_policy_digest IS NULL
        ELSE resolved_policy_run_id IS NOT NULL AND resolved_policy_run_id <> ''
            AND resolved_policy_digest IS NOT NULL AND resolved_policy_digest <> ''
    END)
) STRICT;
CREATE TABLE issue_comment_intents (
    instance_id              TEXT    PRIMARY KEY
        REFERENCES effect_proposal_instances (instance_id),
    repository_id            INTEGER NOT NULL CHECK (repository_id > 0),
    issue_number             INTEGER NOT NULL CHECK (issue_number > 0),
    opened_at                TEXT    NOT NULL CHECK (opened_at <> ''),
    pre_dispatch_comment_ids TEXT,
    pre_dispatch_bot_user_id INTEGER CHECK (pre_dispatch_bot_user_id > 0),
    pre_dispatch_recorded_at TEXT,
    outcome                  TEXT,
    refusal_reason           TEXT,
    resolved_at              TEXT,
    CHECK ((pre_dispatch_comment_ids IS NULL) = (pre_dispatch_recorded_at IS NULL)),
    CHECK ((pre_dispatch_comment_ids IS NULL) = (pre_dispatch_bot_user_id IS NULL)),
    CHECK ((outcome IS NULL) = (resolved_at IS NULL)),
    CHECK ((refusal_reason IS NOT NULL) = (outcome IS NOT NULL AND outcome = 'refused'))
) STRICT;
CREATE TABLE issue_comment_attempts (
    instance_id          TEXT    NOT NULL
        REFERENCES issue_comment_intents (instance_id),
    ordinal              INTEGER NOT NULL CHECK (ordinal >= 1),
    dispatch_started_at  TEXT    NOT NULL CHECK (dispatch_started_at <> ''),
    response_class       TEXT,
    response_comment_id  INTEGER CHECK (response_comment_id > 0),
    response_recorded_at TEXT,
    PRIMARY KEY (instance_id, ordinal),
    CHECK ((response_class IS NULL) = (response_recorded_at IS NULL)),
    CHECK ((response_comment_id IS NOT NULL)
        = (response_class IS NOT NULL AND response_class = 'success'))
) STRICT;
CREATE TABLE posted_issue_comments (
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    comment_id    INTEGER NOT NULL CHECK (comment_id > 0),
    issue_number  INTEGER NOT NULL CHECK (issue_number > 0),
    instance_id   TEXT    NOT NULL UNIQUE
        REFERENCES issue_comment_intents (instance_id),
    body          TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (repository_id, comment_id)
) STRICT;
CREATE TABLE issue_comment_policy_approvals (
    instance_id     TEXT NOT NULL PRIMARY KEY REFERENCES effect_proposal_instances (instance_id),
    proposal_digest TEXT NOT NULL CHECK (proposal_digest <> ''),
    policy_digest   TEXT NOT NULL CHECK (policy_digest <> ''),
    approved_at     TEXT NOT NULL CHECK (approved_at <> '')
) STRICT;
CREATE TABLE instance_settings_revisions (
    revision INTEGER NOT NULL PRIMARY KEY CHECK (revision > 0),
    section  TEXT    NOT NULL CHECK (section <> ''),
    set_by   TEXT    NOT NULL CHECK (set_by <> ''),
    set_at   TEXT    NOT NULL CHECK (set_at <> ''),
    body     TEXT    NOT NULL CHECK (body <> '')
) STRICT;
CREATE TABLE project_settings_revisions (
    project_id TEXT    NOT NULL CHECK (project_id <> ''),
    revision   INTEGER NOT NULL CHECK (revision > 0),
    section    TEXT    NOT NULL CHECK (section <> ''),
    set_by     TEXT    NOT NULL CHECK (set_by <> ''),
    set_at     TEXT    NOT NULL CHECK (set_at <> ''),
    body       TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (project_id, revision)
) STRICT;
CREATE TABLE IF NOT EXISTS "launch_reservations" (
    invocation_id TEXT    NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT    CHECK (run_id IS NULL OR run_id <> ''),
    kind          TEXT    NOT NULL CHECK (kind <> ''),
    memory_mib    INTEGER NOT NULL CHECK (memory_mib > 0),
    overhead_mib  INTEGER NOT NULL CHECK (overhead_mib >= 0),
    reserved_at   TEXT    NOT NULL CHECK (reserved_at <> ''),
    released_at   TEXT    CHECK (released_at IS NULL OR released_at <> '')
) STRICT;
CREATE TABLE IF NOT EXISTS "launch_waits" (
    invocation_id   TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id          TEXT CHECK (run_id IS NULL OR run_id <> ''),
    task_created_at TEXT CHECK (task_created_at IS NULL OR task_created_at <> ''),
    waiting_since   TEXT NOT NULL CHECK (waiting_since <> ''),
    refused_at      TEXT NOT NULL CHECK (refused_at <> ''),
    kept_at         TEXT CHECK (kept_at IS NULL OR kept_at <> ''),
    CHECK ((run_id IS NULL) = (task_created_at IS NULL))
) STRICT;
CREATE TABLE run_hold_spans (
    id                INTEGER PRIMARY KEY,
    run_id            TEXT NOT NULL CHECK (run_id <> ''),
    invocation_id     TEXT CHECK (invocation_id IS NULL OR invocation_id <> ''),
    reason            TEXT NOT NULL CHECK (reason <> ''),
    first_observed_at TEXT NOT NULL CHECK (first_observed_at <> ''),
    last_observed_at  TEXT NOT NULL CHECK (last_observed_at <> ''),
    cleared_at        TEXT NOT NULL CHECK (cleared_at <> '')
) STRICT;
CREATE TABLE project_readiness (
    project_id        TEXT    NOT NULL CHECK (project_id <> ''),
    instance_revision INTEGER NOT NULL CHECK (instance_revision >= 0),
    project_revision  INTEGER NOT NULL CHECK (project_revision >= 0),
    evaluated_at      TEXT    NOT NULL CHECK (evaluated_at <> ''),
    body              TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (project_id, instance_revision, project_revision)
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
CREATE TRIGGER decision_action_surfaces_append_only_update
BEFORE UPDATE ON decision_action_surfaces
BEGIN
    SELECT RAISE(ABORT, 'decision action surfaces are append-only');
END;
CREATE TRIGGER decision_action_surfaces_append_only_delete
BEFORE DELETE ON decision_action_surfaces
BEGIN
    SELECT RAISE(ABORT, 'decision action surfaces are append-only');
END;
CREATE TRIGGER comprehension_events_append_only_update
BEFORE UPDATE ON comprehension_events
BEGIN
    SELECT RAISE(ABORT, 'comprehension events are append-only');
END;
CREATE TRIGGER comprehension_events_append_only_delete
BEFORE DELETE ON comprehension_events
BEGIN
    SELECT RAISE(ABORT, 'comprehension events are append-only');
END;
CREATE TRIGGER comprehension_defects_append_only_update
BEFORE UPDATE ON comprehension_defects
BEGIN
    SELECT RAISE(ABORT, 'comprehension defects are append-only');
END;
CREATE TRIGGER comprehension_defects_append_only_delete
BEFORE DELETE ON comprehension_defects
BEGIN
    SELECT RAISE(ABORT, 'comprehension defects are append-only');
END;
CREATE TRIGGER outbox_publication_intent_requires_current_insert
BEFORE INSERT ON outbox
WHEN NEW.kind = 'publish.publication' AND NEW.payload_version NOT IN (3, 4)
BEGIN
    SELECT RAISE(ABORT, 'new publication intents require current payload version');
END;
CREATE TRIGGER outbox_publication_intent_requires_current_promotion
BEFORE UPDATE OF kind, payload_version ON outbox
WHEN NEW.kind = 'publish.publication'
    AND OLD.kind != 'publish.publication'
    AND NEW.payload_version NOT IN (3, 4)
BEGIN
    SELECT RAISE(ABORT, 'promoted publication intents require current payload version');
END;
CREATE TRIGGER outbox_payload_version_no_downgrade
BEFORE UPDATE OF payload_version ON outbox
WHEN NEW.payload_version < OLD.payload_version
BEGIN
    SELECT RAISE(ABORT, 'outbox payload version cannot be downgraded');
END;
CREATE TRIGGER runs_task_insert BEFORE INSERT ON runs
WHEN NOT EXISTS (SELECT 1 FROM tasks WHERE id = NEW.task_id AND project_id = NEW.project_id)
BEGIN SELECT RAISE(ABORT, 'run task must exist in its project'); END;
CREATE TRIGGER runs_task_update BEFORE UPDATE OF task_id, project_id ON runs
WHEN NOT EXISTS (SELECT 1 FROM tasks WHERE id = NEW.task_id AND project_id = NEW.project_id)
BEGIN SELECT RAISE(ABORT, 'run task must exist in its project'); END;
CREATE TRIGGER follow_up_filing_intents_resolved_update
BEFORE UPDATE ON follow_up_filing_intents
WHEN OLD.outcome IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'a resolved follow-up filing intent is immutable');
END;
CREATE TRIGGER follow_up_filing_intents_recorded_update
BEFORE UPDATE ON follow_up_filing_intents
WHEN NEW.instance_id IS NOT OLD.instance_id
    OR NEW.repository_id IS NOT OLD.repository_id
    OR NEW.opened_at IS NOT OLD.opened_at
    OR (OLD.pre_dispatch_recorded_at IS NOT NULL
        AND (NEW.pre_dispatch_issue_numbers IS NOT OLD.pre_dispatch_issue_numbers
            OR NEW.pre_dispatch_recorded_at IS NOT OLD.pre_dispatch_recorded_at))
BEGIN
    SELECT RAISE(ABORT, 'recorded follow-up filing intent facts are immutable');
END;
CREATE TRIGGER follow_up_filing_intents_append_only_delete
BEFORE DELETE ON follow_up_filing_intents
BEGIN
    SELECT RAISE(ABORT, 'follow-up filing intents are append-only');
END;
CREATE TRIGGER follow_up_filing_attempts_recorded_update
BEFORE UPDATE ON follow_up_filing_attempts
WHEN OLD.response_class IS NOT NULL
    OR NEW.instance_id IS NOT OLD.instance_id
    OR NEW.ordinal IS NOT OLD.ordinal
    OR NEW.dispatch_started_at IS NOT OLD.dispatch_started_at
BEGIN
    SELECT RAISE(ABORT, 'recorded follow-up filing attempt facts are immutable');
END;
CREATE TRIGGER follow_up_filing_attempts_append_only_delete
BEFORE DELETE ON follow_up_filing_attempts
BEGIN
    SELECT RAISE(ABORT, 'follow-up filing attempts are append-only');
END;
CREATE TRIGGER follow_up_filed_issues_append_only_update
BEFORE UPDATE ON follow_up_filed_issues
BEGIN
    SELECT RAISE(ABORT, 'filed follow-up issues are append-only');
END;
CREATE TRIGGER follow_up_filed_issues_append_only_delete
BEFORE DELETE ON follow_up_filed_issues
BEGIN
    SELECT RAISE(ABORT, 'filed follow-up issues are append-only');
END;
CREATE TRIGGER follow_up_filing_intents_identity_update
BEFORE UPDATE ON follow_up_filing_intents
WHEN OLD.pre_dispatch_recorded_at IS NOT NULL
    AND NEW.pre_dispatch_bot_user_id IS NOT OLD.pre_dispatch_bot_user_id
BEGIN
    SELECT RAISE(ABORT, 'a recorded follow-up filing dispatching identity is immutable');
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
CREATE TRIGGER issue_comment_intents_resolved_update
BEFORE UPDATE ON issue_comment_intents
WHEN OLD.outcome IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'a resolved issue comment intent is immutable');
END;
CREATE TRIGGER issue_comment_intents_recorded_update
BEFORE UPDATE ON issue_comment_intents
WHEN NEW.instance_id IS NOT OLD.instance_id
    OR NEW.repository_id IS NOT OLD.repository_id
    OR NEW.issue_number IS NOT OLD.issue_number
    OR NEW.opened_at IS NOT OLD.opened_at
    OR (OLD.pre_dispatch_recorded_at IS NOT NULL
        AND (NEW.pre_dispatch_comment_ids IS NOT OLD.pre_dispatch_comment_ids
            OR NEW.pre_dispatch_bot_user_id IS NOT OLD.pre_dispatch_bot_user_id
            OR NEW.pre_dispatch_recorded_at IS NOT OLD.pre_dispatch_recorded_at))
BEGIN
    SELECT RAISE(ABORT, 'recorded issue comment intent facts are immutable');
END;
CREATE TRIGGER issue_comment_intents_append_only_delete
BEFORE DELETE ON issue_comment_intents
BEGIN
    SELECT RAISE(ABORT, 'issue comment intents are append-only');
END;
CREATE TRIGGER issue_comment_attempts_recorded_update
BEFORE UPDATE ON issue_comment_attempts
WHEN OLD.response_class IS NOT NULL
    OR NEW.instance_id IS NOT OLD.instance_id
    OR NEW.ordinal IS NOT OLD.ordinal
    OR NEW.dispatch_started_at IS NOT OLD.dispatch_started_at
BEGIN
    SELECT RAISE(ABORT, 'recorded issue comment attempt facts are immutable');
END;
CREATE TRIGGER issue_comment_attempts_append_only_delete
BEFORE DELETE ON issue_comment_attempts
BEGIN
    SELECT RAISE(ABORT, 'issue comment attempts are append-only');
END;
CREATE TRIGGER posted_issue_comments_append_only_update
BEFORE UPDATE ON posted_issue_comments
BEGIN
    SELECT RAISE(ABORT, 'posted issue comments are append-only');
END;
CREATE TRIGGER posted_issue_comments_append_only_delete
BEFORE DELETE ON posted_issue_comments
BEGIN
    SELECT RAISE(ABORT, 'posted issue comments are append-only');
END;
CREATE TRIGGER external_finding_disposition_requires_admitted_cycle
BEFORE INSERT ON external_finding_dispositions
WHEN NOT EXISTS (
    SELECT 1
    FROM findings AS raw_finding
    JOIN review_records AS record
      ON record.run_id = raw_finding.run_id
     AND record.round = NEW.round
    WHERE raw_finding.id = NEW.finding_id
      AND raw_finding.run_id = NEW.run_id
      AND json_extract(raw_finding.body, '$.id') = raw_finding.id
      AND json_extract(raw_finding.body, '$.run_id') = raw_finding.run_id
      AND json_type(raw_finding.body, '$.external') = 'object'
      AND json_extract(record.body, '$.run_id') = record.run_id
      AND json_extract(record.body, '$.round') = record.round
      AND json_extract(record.body, '$.base_sha') = record.base_sha
      AND json_extract(record.body, '$.head_sha') = record.head_sha
      AND record.head_sha = json_extract(raw_finding.body, '$.external.head_sha')
      AND EXISTS (
          SELECT 1
          FROM outbox AS authority
          WHERE authority.kind = 'publication_successor_authority'
            AND authority.status = 'dispatched'
            AND CASE WHEN json_valid(CAST(authority.payload AS TEXT)) THEN
                    json_extract(CAST(authority.payload AS TEXT), '$.run_id') = record.run_id
                    AND json_extract(CAST(authority.payload AS TEXT), '$.origin') IN ('external_review', 'hand_back')
                    AND json_extract(CAST(authority.payload AS TEXT), '$.review_round') = record.round
                    AND json_extract(CAST(authority.payload AS TEXT), '$.reentry.base_sha') = record.base_sha
                    AND json_extract(CAST(authority.payload AS TEXT), '$.reentry.head_sha') = record.head_sha
                ELSE 0 END
      )
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
)
BEGIN
    SELECT RAISE(ABORT, 'external finding does not belong to an external review cycle round');
END;
CREATE INDEX execution_admissions_run ON execution_admissions (run_id);
CREATE INDEX project_images_repository ON project_images (repository_id);
CREATE INDEX attention_items_open_by_type
    ON attention_items (item_type) WHERE status = 'open';
CREATE INDEX backend_conformance_by_backend
    ON backend_conformance_records (backend, id);
CREATE INDEX trust_profile_activations_repo_id
    ON trust_profile_activations(repo, id);
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
CREATE INDEX decision_action_surfaces_item
    ON decision_action_surfaces (item_id);
CREATE INDEX comprehension_events_item
    ON comprehension_events (item_id);
CREATE UNIQUE INDEX run_milestones_identity
    ON run_milestones (run_id, kind, COALESCE(invocation_id, ''));
CREATE INDEX runs_by_parent ON runs (parent_run_id);
CREATE INDEX ready_item_pr_bindings_run ON ready_item_pr_bindings(run_id);
CREATE INDEX runs_task ON runs(task_id);
CREATE INDEX production_attempts_revises_run_id
    ON production_attempts (revises_run_id)
    WHERE revises_run_id IS NOT NULL;
CREATE INDEX publication_authorings_by_run
    ON publication_authorings (run_id, created_at, content_digest);
CREATE UNIQUE INDEX effect_proposal_items_task_digest
    ON effect_proposal_items (instance_id, content_digest)
    WHERE candidate_head_sha IS NULL;
CREATE INDEX auth_store_read_holds_unreleased
    ON auth_store_read_holds (auth_identity_id, expires_at_unix_nano)
    WHERE released_at IS NULL;
CREATE INDEX drift_audits_by_digest
    ON drift_audits (content_digest);
CREATE INDEX held_item_pr_bindings_run ON held_item_pr_bindings(run_id);
CREATE UNIQUE INDEX follow_up_filing_intents_one_outstanding
    ON follow_up_filing_intents (repository_id)
    WHERE outcome IS NULL;
CREATE INDEX follow_up_filing_intents_repository
    ON follow_up_filing_intents (repository_id);
CREATE INDEX external_finding_dispositions_by_run
    ON external_finding_dispositions (run_id, round, finding_id);
CREATE INDEX usage_observations_run
    ON usage_observations (run_id);
CREATE INDEX usage_observations_treatment
    ON usage_observations (treatment_digest);
CREATE INDEX schedules_due ON schedules (status, fire_at);
CREATE INDEX image_upkeep_refusals_repository ON image_upkeep_refusals (repository_id);
CREATE INDEX image_policy_adoptions_repository ON image_policy_adoptions (repository_id);
CREATE INDEX duplicate_searches_by_digest
    ON duplicate_searches (content_digest);
CREATE INDEX effect_proposal_instances_batch
    ON effect_proposal_instances (proposal_batch_id, instance_id);
CREATE UNIQUE INDEX issue_comment_intents_one_outstanding
    ON issue_comment_intents (repository_id, issue_number)
    WHERE outcome IS NULL;
CREATE INDEX launch_reservations_open ON launch_reservations (released_at);
CREATE INDEX run_hold_spans_by_run ON run_hold_spans (run_id);
COMMIT;
