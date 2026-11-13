package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// StoredRowReport is the outcome of DecodeStoredRows. It names each kind of
// row that failed to decode and counts the rest; it never carries a row's
// content or the underlying error, whose text can quote stored values, so it
// is safe to print from a check run on a copy of a credential-bearing
// database.
type StoredRowReport struct {
	// Failed lists the kinds whose walk returned an error, in registry order.
	Failed []string
	// Walked counts the kinds the walk visited, failed ones included.
	Walked int
	// NotWalked counts the tables the walk deliberately skips.
	NotWalked int
}

// DecodeStoredRows reads every stored row through the store's own reads,
// which run each row's reconstruction gates under this store's options, and
// reports which kinds of row failed. A failed kind does not stop the walk, so
// one bad kind never hides another, and a read that panics fails its kind.
// The only error it returns is the context's: a failed read is a finding, not
// an error.
func (s *Store) DecodeStoredRows(ctx context.Context) (StoredRowReport, error) {
	report := StoredRowReport{NotWalked: len(notWalkedTables)}
	for _, kind := range storedRowKinds {
		if err := ctx.Err(); err != nil {
			return StoredRowReport{}, err
		}
		report.Walked++
		if err := kind.run(ctx, s); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return StoredRowReport{}, ctxErr
			}
			report.Failed = append(report.Failed, kind.name)
		}
	}
	return report, nil
}

// run walks the kind, turning a panic in a read into the kind's failure: a
// read that panics on a stored row would panic the daemon too, and the check
// must still clean up and report rather than crash.
func (kind storedRowKind) run(ctx context.Context, s *Store) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("kind %q panicked", kind.name)
		}
	}()
	return kind.walk(ctx, s)
}

// storedRowKind is one kind of stored row: the tables its walk covers and the
// walk itself, which calls existing reads only. A child table a parent read
// already decodes is listed under the parent's kind.
type storedRowKind struct {
	name   string
	tables []string
	walk   func(context.Context, *Store) error
}

// notWalkedTables names each table the walk skips and why. Every table in a
// migrated schema is either covered by a storedRowKind or listed here;
// TestStoredRowKindsCoverEveryTable fails otherwise, so a new table must pick
// one.
var notWalkedTables = map[string]string{
	"schema_migrations":               "the migration ledger, which Open validates before any read",
	"legacy_candidate_authorizations": "retired authorizations kept for audit; no read decodes them",
	"local_backup_checkpoint_marker":  "marks a backup file and is read only from a backup, never the live database",
	"local_backup_restore_marker":     "marks a backup file and is read only from a backup, never the live database",
}

// rowKey holds one row's key columns, scanned by a shape such as "sn": each
// 's' fills the next string and each 'n' the next integer.
type rowKey struct {
	s [3]string
	n [3]int64
}

// storedKeys runs query and scans each row by shape. It collects every key
// before returning, so a visit never runs a read while this cursor is open.
func (tx *ReadTx) storedKeys(ctx context.Context, query, shape string) ([]rowKey, error) {
	rows, err := tx.tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []rowKey
	for rows.Next() {
		var key rowKey
		dests := make([]any, 0, len(shape))
		var strings, ints int
		for _, column := range shape {
			switch column {
			case 's':
				dests = append(dests, &key.s[strings])
				strings++
			case 'n':
				dests = append(dests, &key.n[ints])
				ints++
			default:
				return nil, fmt.Errorf("key shape %q: unknown column %q", shape, column)
			}
		}
		if err := rows.Scan(dests...); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// inRead runs fn in one read transaction.
func inRead(fn func(context.Context, *ReadTx) error) func(context.Context, *Store) error {
	return func(ctx context.Context, s *Store) error {
		return s.Read(ctx, func(tx *ReadTx) error { return fn(ctx, tx) })
	}
}

// eachKey visits every key query returns, in one read transaction.
func eachKey(query, shape string, visit func(context.Context, *ReadTx, rowKey) error) func(context.Context, *Store) error {
	return inRead(func(ctx context.Context, tx *ReadTx) error {
		keys, err := tx.storedKeys(ctx, query, shape)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := visit(ctx, tx, key); err != nil {
				return err
			}
		}
		return nil
	})
}

// discard drops a read's values and keeps its error.
func discard[T any](_ T, err error) error { return err }

func discard2[T, U any](_ T, _ U, err error) error { return err }

func discard3[T, U, V any](_ T, _ U, _ V, err error) error { return err }

var storedRowKinds = []storedRowKind{
	// Server and operator state.
	{"server state", []string{"server_state"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.ServerState(ctx))
	})},
	{"memory budget", []string{"memory_budget"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard2(tx.MemoryBudget(ctx))
	})},
	{"routing policy", []string{"routing_policies"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.RoutingPolicyRevisions(ctx))
	})},
	{"workflow policy", []string{"workflow_policy_revisions"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.WorkflowPolicyLayers(ctx))
	})},
	{"unattended operation transition", []string{"unattended_operation_transitions"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard2(tx.LatestUnattendedOperationTransition(ctx))
	})},
	{"update drain transition", []string{"update_drain_transitions"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.UpdateDrainInForce(ctx))
	})},
	{"outbox entry", []string{"outbox"}, eachKey(`SELECT idempotency_key FROM outbox`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error { return discard(tx.GetOutbox(ctx, k.s[0])) })},
	{"inbox entry", []string{"inbox"}, eachKey(`SELECT idempotency_key FROM inbox`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error { return discard(tx.GetInbox(ctx, k.s[0])) })},
	{"project", []string{"projects"}, eachKey(`SELECT project_id FROM projects`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetProject(ctx, domain.ProjectID(k.s[0])))
		})},
	{"manual submission", []string{"manual_submissions"}, eachKey(`SELECT identity FROM manual_submissions`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetManualSubmission(ctx, k.s[0]))
		})},

	// Devices, pairing, and client enrollment.
	{"device", []string{"devices"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		devices, err := tx.ListDevices(ctx)
		if err != nil {
			return err
		}
		for _, device := range devices {
			if err := discard2(tx.GetDeviceSnapshot(ctx, device.Value.ID)); err != nil {
				return err
			}
		}
		return nil
	})},
	{"device credential", []string{"device_credentials"}, eachKey(`SELECT device_id FROM device_credentials`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetDeviceCredential(ctx, domain.DeviceID(k.s[0])))
		})},
	{"device capability contract", []string{"device_capability_contracts"}, eachKey(`SELECT device_id FROM device_capability_contracts`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetDeviceCapabilityContract(ctx, domain.DeviceID(k.s[0])))
		})},
	{"device notification preference", []string{"device_notification_preferences"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.ListDeviceNotificationPreferences(ctx))
	})},
	{"device activity", []string{"device_activity"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.ListDeviceActivity(ctx))
	})},
	{"pairing code", []string{"pairing_codes"}, eachKey(`SELECT code_hash FROM pairing_codes`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetPairingCode(ctx, domain.Digest(k.s[0])))
		})},
	{"client enrollment", []string{"client_enrollments"}, eachKey(`SELECT id FROM client_enrollments`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetClientEnrollment(ctx, domain.ClientEnrollmentID(k.s[0])))
		})},
	{"client enrollment generation", []string{"client_enrollment_generations"}, eachKey(`SELECT enrollment_id, ordinal FROM client_enrollment_generations`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetEnrollmentGeneration(ctx, domain.ClientEnrollmentID(k.s[0]), int(k.n[0])))
		})},
	{"generation integrity mark", []string{"generation_integrity_marks"}, eachKey(`SELECT DISTINCT enrollment_id, ordinal FROM generation_integrity_marks`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GenerationIntegrityMarks(ctx, domain.ClientEnrollmentID(k.s[0]), int(k.n[0])))
		})},
	{"discovered model catalog", []string{"discovered_model_catalogs"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.ListDiscoveredModelCatalogs(ctx))
	})},
	{"comprehension", []string{"comprehension_events", "comprehension_defects"}, decodeComprehensionRows},

	// Provider credentials and usage.
	{"auth identity", []string{"auth_identities"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		identities, err := tx.ListAuthIdentities(ctx)
		if err != nil {
			return err
		}
		for _, identity := range identities {
			if err := discard(tx.GetAuthIdentity(ctx, identity.ID)); err != nil {
				return err
			}
		}
		return nil
	})},
	{"auth store mutation lease", []string{"auth_store_mutation_leases"}, eachKey(`SELECT auth_identity_id FROM auth_store_mutation_leases`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetAuthStoreMutationLease(ctx, domain.AuthIdentityID(k.s[0])))
		})},
	{"auth store read hold", []string{"auth_store_read_holds"}, eachKey(`SELECT auth_identity_id, holder FROM auth_store_read_holds`, "ss",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetAuthStoreReadHold(ctx, domain.AuthIdentityID(k.s[0]), domain.InvocationID(k.s[1])))
		})},
	{"codex reenrollment", []string{"codex_reenrollment_operations"}, eachKey(`SELECT auth_identity_id, lease_fence FROM codex_reenrollment_operations`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetCodexReenrollmentJournal(ctx, domain.AuthIdentityID(k.s[0]), k.n[0]))
		})},
	{"codex reenrollment recovery transition", []string{"codex_reenrollment_recovery_transitions"}, eachKey(`SELECT DISTINCT auth_identity_id FROM codex_reenrollment_recovery_transitions`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.LatestCodexReenrollmentRecoveryTransition(ctx, domain.AuthIdentityID(k.s[0])))
		})},
	{"usage pool", []string{"usage_pools"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		pools, err := tx.ListUsagePools(ctx)
		if err != nil {
			return err
		}
		for _, pool := range pools {
			if err := discard(tx.GetUsagePool(ctx, pool.ID)); err != nil {
				return err
			}
		}
		return nil
	})},
	{"pool usage observation", []string{"pool_usage_observations"}, decodePoolUsageObservationRows},
	{"usage observation", []string{"usage_observations"}, decodeUsageObservationRows},
	{"mint audit", []string{"publish_mint_audits"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.ListMintAudits(ctx))
	})},
	{"installation mint audit", []string{"publish_installation_mint_audits"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.ListInstallationMintAudits(ctx))
	})},
	{"installation mint hold", []string{"publish_installation_mint_holds"}, eachKey(`SELECT installation_id FROM publish_installation_mint_holds`, "n",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.InstallationMintHold(ctx, k.n[0]))
		})},

	// Conformance, trust, and recipes.
	{"adapter conformance", []string{"adapter_conformance_records"}, eachKey(`SELECT DISTINCT adapter_digest FROM adapter_conformance_records`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.LatestAdapterConformance(ctx, domain.Digest(k.s[0])))
		})},
	{"backend conformance", []string{"backend_conformance_records"}, eachKey(`SELECT DISTINCT backend FROM backend_conformance_records`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.LatestBackendConformance(ctx, domain.RunnerBackendClass(k.s[0])))
		})},
	{"trust profile", []string{"trust_profiles"}, eachKey(`SELECT profile_digest FROM trust_profiles`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetTrustProfile(ctx, domain.Digest(k.s[0])))
		})},
	{"trust profile activation", []string{"trust_profile_activations"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		inspections, err := tx.InspectLatestTrustProfiles(ctx)
		if err != nil {
			return err
		}
		for _, inspection := range inspections {
			if inspection.ReconstructionError != nil {
				return inspection.ReconstructionError
			}
		}
		return nil
	})},
	{"workflow audit", []string{"workflow_audits"}, eachKey(`SELECT id FROM workflow_audits`, "n",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetWorkflowAudit(ctx, k.n[0]))
		})},
	{"workflow audit evidence", []string{"workflow_audit_evidence"}, eachKey(`SELECT repo, workflow_audit_digest FROM workflow_audit_evidence`, "ss",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.workflowAuditEvidence(ctx, k.s[0], domain.Digest(k.s[1])))
		})},
	{"candidate authorization", []string{"candidate_authorizations"}, eachKey(`SELECT id FROM candidate_authorizations`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetCandidateAuthorization(ctx, domain.Digest(k.s[0])))
		})},
	{"shadow review configuration approval", []string{"shadow_review_configuration_approvals"}, eachKey(`SELECT approval_digest FROM shadow_review_configuration_approvals`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetShadowReviewConfigurationApproval(ctx, domain.Digest(k.s[0])))
		})},
	{"shadow review configuration activation", []string{"shadow_review_configuration_activations"}, eachKey(`SELECT DISTINCT repo, source FROM shadow_review_configuration_activations`, "ss",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			inspection, err := tx.InspectCurrentShadowReviewConfigurationApproval(ctx, k.s[0], domain.ShadowReviewSource(k.s[1]))
			if err != nil {
				return err
			}
			return inspection.ReconstructionError
		})},
	{"check proof", []string{"check_proofs"}, eachKey(`SELECT digest FROM check_proofs`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetCheckProof(ctx, domain.Digest(k.s[0])))
		})},
	{"requirement resolution", []string{"requirement_resolutions"}, eachKey(`SELECT digest FROM requirement_resolutions`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetRequirementResolution(ctx, domain.Digest(k.s[0])))
		})},
	{"degraded waiver", []string{"degraded_waivers"}, eachKey(`SELECT waiver_id FROM degraded_waivers`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetValidatedDegradedWaiver(ctx, domain.WaiverID(k.s[0])))
		})},
	{"waiver lifecycle event", []string{"waiver_lifecycle_events"}, eachKey(`SELECT waiver_id, sequence FROM waiver_lifecycle_events`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.getWaiverLifecycleEvent(ctx, domain.WaiverID(k.s[0]), uint64(k.n[0])))
		})},

	// Project images.
	{"project image", []string{"project_images"}, eachKey(`SELECT id FROM project_images`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetProjectImage(ctx, domain.Digest(k.s[0])))
		})},
	{"admitted image pointer", []string{"admitted_image_pointers"}, eachKey(`SELECT repository_id, harness FROM admitted_image_pointers`, "ns",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard3(tx.GetAdmittedImagePointer(ctx, k.n[0], domain.HarnessClientKind(k.s[0])))
		})},
	{"image upkeep refusal", []string{"image_upkeep_refusals"}, eachKey(`SELECT id FROM image_upkeep_refusals`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetImageUpkeepRefusal(ctx, domain.Digest(k.s[0])))
		})},
	{"image policy adoption", []string{"image_policy_adoptions"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.imagePolicyAdoptions(ctx))
	})},

	// Repository observations.
	{"repository tip observation", []string{"repository_tip_observations"}, eachKey(`SELECT repository_id FROM repository_tip_observations`, "n",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.GetRepositoryTipObservation(ctx, k.n[0]))
		})},
	{"issue state fact", []string{"issue_state_facts"}, eachKey(`SELECT DISTINCT repository_id, issue_number FROM issue_state_facts`, "nn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.ListIssueStateFacts(ctx, k.n[0], int(k.n[1])))
		})},
	{"pull merge fact", []string{"pull_merge_facts"}, eachKey(`SELECT DISTINCT repository_id, pr_number FROM pull_merge_facts`, "nn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.ListPullMergeFacts(ctx, k.n[0], int(k.n[1])))
		})},
	{"native review observation", []string{"native_review_observations"}, eachKey(`SELECT DISTINCT repository_id, pr_number FROM native_review_observations`, "nn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.ListNativeReviewObservations(ctx, k.n[0], int(k.n[1])))
		})},
	{"intake occurrence", []string{"intake_occurrences"}, eachKey(`SELECT repository_id, issue_number, label, ordinal FROM intake_occurrences`, "nnsn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetIntakeOccurrence(ctx, k.n[0], int(k.n[1]), k.s[0], int(k.n[2])))
		})},

	// Schedules.
	{"schedule", []string{"schedules"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		schedules, err := tx.ListSchedules(ctx)
		if err != nil {
			return err
		}
		for _, schedule := range schedules {
			if err := discard2(tx.GetScheduleSnapshot(ctx, schedule.Value.ID)); err != nil {
				return err
			}
		}
		return nil
	})},
	{"schedule timer", []string{"schedule_timers"}, eachKey(`SELECT schedule_id FROM schedule_timers`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			_, _, _, err := tx.GetScheduleTimer(ctx, domain.ScheduleID(k.s[0]))
			return err
		})},
	{"schedule occurrence", []string{"schedule_occurrences"}, eachKey(`SELECT schedule_id, generation, nominal_fire_at FROM schedule_occurrences`, "sns",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			nominal, err := parseTime(k.s[1])
			if err != nil {
				return err
			}
			return discard(tx.GetScheduleOccurrence(ctx, domain.ScheduleID(k.s[0]), k.n[0], nominal))
		})},

	// Tasks.
	{"task", []string{"tasks", "task_lifecycle_facts"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		tasks, err := tx.ListTasks(ctx)
		if err != nil {
			return err
		}
		for _, task := range tasks {
			if err := discard(tx.GetTaskSnapshot(ctx, task.Value.ID)); err != nil {
				return err
			}
		}
		return nil
	})},
	{"task intake key", []string{"task_intake_keys"}, eachKey(`SELECT project_id, intake_key FROM task_intake_keys`, "ss",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetTaskByIntakeKey(ctx, domain.ProjectID(k.s[0]), k.s[1]))
		})},
	{"task attachment", []string{"task_attachments"}, eachKey(`SELECT DISTINCT task_id FROM task_attachments`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.TaskAttachments(ctx, domain.TaskID(k.s[0])))
		})},
	{"task line", []string{"task_lines"}, eachKey(`SELECT id FROM task_lines`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetTaskLine(ctx, domain.Digest(k.s[0])))
		})},
	{"task line change", []string{"task_line_changes"}, eachKey(`SELECT id FROM task_line_changes`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetTaskLineChange(ctx, domain.Digest(k.s[0])))
		})},
	{"task run", []string{"task_runs"}, eachKey(`SELECT DISTINCT task_id FROM task_runs`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.TaskRunIDs(ctx, domain.TaskID(k.s[0])))
		})},
	{"task triage", []string{"task_triages"}, eachKey(`SELECT task_id FROM task_triages`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.GetTaskTriage(ctx, domain.TaskID(k.s[0])))
		})},
	{"task cancellation", []string{"task_cancellations"}, eachKey(`SELECT request_id FROM task_cancellations`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetTaskCancellation(ctx, k.s[0]))
		})},
	{"task cancellation acknowledgement", []string{"task_cancellation_acknowledgements"}, eachKey(`SELECT request_id, as_of_revision FROM task_cancellation_acknowledgements`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.cancellationAtRevision(ctx, k.s[0], k.n[0]))
		})},
	{"task stop command", []string{"task_stop_commands"}, eachKey(`SELECT command_id FROM task_stop_commands`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.GetStopTaskReceipt(ctx, k.s[0]))
		})},
	{"task submission", []string{"task_submission_commands"}, eachKey(`SELECT command_id FROM task_submission_commands`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.GetTaskSubmissionSnapshot(ctx, k.s[0]))
		})},
	{"task workflow closure", []string{"task_workflow_closures"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.ListTaskWorkflowClosures(ctx))
	})},
	{"task attention workflow origin", []string{"task_attention_workflow_origins"}, eachKey(`SELECT item_id FROM task_attention_workflow_origins`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			item, err := tx.GetAttentionItemRecord(ctx, domain.ItemID(k.s[0]))
			if err != nil {
				return err
			}
			return discard(tx.taskAttentionWorkflowRun(ctx, item))
		})},
	{"alternate agent retry", []string{"alternate_agent_retries"}, eachKey(`SELECT id FROM alternate_agent_retries`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetAlternateAgentRetry(ctx, domain.Digest(k.s[0])))
		})},

	// Runs, conversations, and artifacts.
	{"run", []string{"runs"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		runs, err := tx.ListRuns(ctx)
		if err != nil {
			return err
		}
		for _, run := range runs {
			if err := discard(tx.GetRunSnapshot(ctx, run.Value.ID)); err != nil {
				return err
			}
		}
		return nil
	})},
	{"conversation", []string{"conversations"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		conversations, err := tx.ListConversations(ctx)
		if err != nil {
			return err
		}
		for _, conversation := range conversations {
			if err := discard(tx.GetConversation(ctx, conversation.Value.ID)); err != nil {
				return err
			}
		}
		return nil
	})},
	{"artifact", []string{"artifacts"}, eachKey(`SELECT id FROM artifacts`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetArtifact(ctx, domain.ArtifactID(k.s[0])))
		})},
	{"resolved policy", []string{"resolved_policies"}, eachKey(`SELECT run_id FROM resolved_policies`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetResolvedPolicy(ctx, domain.RunID(k.s[0])))
		})},
	{"run milestone", []string{"run_milestones"}, eachKey(`SELECT DISTINCT run_id FROM run_milestones`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.ListRunMilestones(ctx, domain.RunID(k.s[0])))
		})},
	{"run hold", []string{"run_hold_observations"}, eachKey(`SELECT run_id FROM run_hold_observations`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.GetRunHold(ctx, domain.RunID(k.s[0])))
		})},
	{"production attempt", []string{"production_attempts"}, eachKey(`SELECT campaign_id, attempt_number FROM production_attempts`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetProductionAttempt(ctx, domain.CampaignID(k.s[0]), int(k.n[0])))
		})},
	{"work unit declaration", []string{"work_unit_declarations"}, eachKey(`SELECT unit_id FROM work_unit_declarations`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetWorkUnitDeclaration(ctx, domain.WorkUnitID(k.s[0])))
		})},
	{"work unit completion", []string{"work_unit_completions"}, eachKey(`SELECT unit_id FROM work_unit_completions`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetWorkUnitCompletion(ctx, domain.WorkUnitID(k.s[0])))
		})},
	{"work unit PR binding", []string{"work_unit_pr_bindings"}, eachKey(`SELECT unit_id FROM work_unit_pr_bindings`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetWorkUnitPRBinding(ctx, domain.WorkUnitID(k.s[0])))
		})},
	{"work unit rebase proof", []string{"work_unit_rebase_proofs"}, eachKey(`SELECT unit_id, proven_head_sha FROM work_unit_rebase_proofs`, "ss",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetWorkUnitRebaseProof(ctx, domain.WorkUnitID(k.s[0]), k.s[1]))
		})},

	// Invocations and execution.
	{"agent invocation", []string{"agent_invocations"}, eachKey(`SELECT id FROM agent_invocations`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetAgentInvocation(ctx, domain.InvocationID(k.s[0])))
		})},
	{"agent claim", []string{"agent_claims"}, eachKey(`SELECT invocation_id FROM agent_claims`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetAgentClaims(ctx, domain.InvocationID(k.s[0])))
		})},
	{"invocation observation", []string{"invocation_observations"}, eachKey(`SELECT DISTINCT run_id FROM invocation_observations`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.ListInvocationObservations(ctx, domain.RunID(k.s[0])))
		})},
	{"execution admission", []string{"execution_admissions"}, eachKey(`SELECT invocation_id FROM execution_admissions`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetExecutionAdmissionRecord(ctx, domain.InvocationID(k.s[0])))
		})},
	{"execution export", []string{"execution_exports"}, eachKey(`SELECT invocation_id FROM execution_exports`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetExecutionExportRecord(ctx, domain.InvocationID(k.s[0])))
		})},
	{"execution outcome", []string{"execution_outcomes"}, eachKey(`SELECT invocation_id FROM execution_outcomes`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetExecutionOutcomeRecord(ctx, domain.InvocationID(k.s[0])))
		})},
	{"export rejection", []string{"export_rejections"}, eachKey(`SELECT invocation_id FROM export_rejections`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetExportRejection(ctx, domain.InvocationID(k.s[0])))
		})},
	{"current import start", []string{"current_import_starts"}, eachKey(`SELECT invocation_id FROM current_import_starts`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetCurrentImportStart(ctx, domain.InvocationID(k.s[0])))
		})},
	{"launch reservation", []string{"launch_reservations"}, eachKey(`SELECT invocation_id FROM launch_reservations`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.getLaunchReservation(ctx, domain.InvocationID(k.s[0])))
		})},
	{"launch wait", []string{"launch_waits"}, eachKey(`SELECT invocation_id FROM launch_waits`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.LaunchWaitRefusedAt(ctx, domain.InvocationID(k.s[0])))
		})},
	{"handoff journal", []string{"handoff_journal_records"}, eachKey(`SELECT run_id FROM handoff_journal_records`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetHandoffJournal(ctx, k.s[0]))
		})},
	{"publication handoff check", []string{"publication_handoff_checks"}, eachKey(`SELECT run_id, publication_id FROM publication_handoff_checks`, "ss",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetHandoffCheck(ctx, domain.RunID(k.s[0]), domain.InvocationID(k.s[1])))
		})},
	{"publication authoring", []string{"publication_authorings"}, eachKey(`SELECT content_digest FROM publication_authorings`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetPublicationAuthoring(ctx, domain.Digest(k.s[0])))
		})},

	// Review.
	{"review admission", []string{"review_admissions"}, eachKey(`SELECT invocation_id FROM review_admissions`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetReviewAdmission(ctx, domain.InvocationID(k.s[0])))
		})},
	{"review slot release", []string{"review_slot_releases"}, eachKey(`SELECT invocation_id FROM review_slot_releases`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.ReviewSlotReleased(ctx, domain.InvocationID(k.s[0])))
		})},
	{"review request", []string{"review_requests"}, eachKey(`SELECT invocation_id FROM review_requests`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetReviewRequest(ctx, domain.InvocationID(k.s[0])))
		})},
	{"review record", []string{"review_records", "review_record_findings"}, eachKey(`SELECT invocation_id FROM review_records`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetReviewRecord(ctx, domain.InvocationID(k.s[0])))
		})},
	{"review failure", []string{"review_failures"}, eachKey(`SELECT invocation_id FROM review_failures`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetReviewFailure(ctx, domain.InvocationID(k.s[0])))
		})},
	{"review retry", []string{"review_retries"}, eachKey(`SELECT run_id FROM review_retries`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetReviewRetry(ctx, domain.RunID(k.s[0])))
		})},
	{"review recovery transition", []string{"review_recovery_transitions"}, eachKey(`SELECT DISTINCT run_id FROM review_recovery_transitions`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.LatestReviewRecoveryTransition(ctx, domain.RunID(k.s[0])))
		})},
	{"review configuration recovery transition", []string{"review_configuration_recovery_transitions"}, eachKey(`SELECT DISTINCT run_id FROM review_configuration_recovery_transitions`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.LatestReviewConfigurationRecoveryTransition(ctx, domain.RunID(k.s[0])))
		})},
	{"review round diff metrics", []string{"review_round_diff_metrics"}, eachKey(`SELECT run_id, round FROM review_round_diff_metrics`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetReviewRoundDiffMetrics(ctx, domain.RunID(k.s[0]), int(k.n[0])))
		})},
	{"shadow review record", []string{"shadow_review_records", "shadow_review_record_findings"}, eachKey(`SELECT invocation_id FROM shadow_review_records`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetShadowReviewRecord(ctx, domain.InvocationID(k.s[0])))
		})},
	{"classifier accuracy sample", []string{"classifier_accuracy_samples"}, eachKey(`SELECT shadow_invocation_id, finding_id, classification_version FROM classifier_accuracy_samples`, "ssn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.getClassifierAccuracySample(ctx, domain.InvocationID(k.s[0]), domain.FindingID(k.s[1]), int(k.n[0])))
		})},
	{"drift audit", []string{"drift_audits"}, eachKey(`SELECT run_id, round FROM drift_audits`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetDriftAuditForRound(ctx, domain.RunID(k.s[0]), int(k.n[0])))
		})},
	{"duplicate search", []string{"duplicate_searches"}, eachKey(`SELECT adjudication_digest, finding_id FROM duplicate_searches`, "ss",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetDuplicateSearch(ctx, domain.Digest(k.s[0]), domain.FindingID(k.s[1])))
		})},
	{"finding", []string{"findings"}, eachKey(`SELECT id FROM findings`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetFinding(ctx, domain.FindingID(k.s[0])))
		})},
	{"classification", []string{"classifications"}, eachKey(`SELECT finding_id, version FROM classifications`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetClassification(ctx, domain.FindingID(k.s[0]), int(k.n[0])))
		})},
	{"finding adjudication", []string{"finding_adjudications"}, eachKey(`SELECT run_id, round, revision FROM finding_adjudications`, "snn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.getFindingAdjudicationRevision(ctx, domain.RunID(k.s[0]), int(k.n[0]), int(k.n[1])))
		})},
	{"finding disposition", []string{"finding_dispositions"}, eachKey(`SELECT finding_id, round FROM finding_dispositions`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetFindingDisposition(ctx, domain.FindingID(k.s[0]), int(k.n[0])))
		})},
	{"external finding disposition", []string{"external_finding_dispositions"}, eachKey(`SELECT finding_id, round FROM external_finding_dispositions`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetExternalFindingDisposition(ctx, domain.FindingID(k.s[0]), int(k.n[0])))
		})},
	{"finding disposition supersession", []string{"finding_disposition_supersessions"}, eachKey(`SELECT finding_id, superseded_round FROM finding_disposition_supersessions`, "sn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetFindingDispositionSupersession(ctx, domain.FindingID(k.s[0]), int(k.n[0])))
		})},
	{"codex review", []string{
		"codex_review_bindings", "codex_review_intents", "codex_review_outcomes",
		"codex_review_requests", "codex_review_workspaces",
	}, inRead(func(ctx context.Context, tx *ReadTx) error {
		for _, table := range []struct {
			query string
			get   func(context.Context, string) (CodexReviewOpaqueRecord, error)
		}{
			{`SELECT run_id FROM codex_review_bindings`, tx.GetCodexReviewBinding},
			{`SELECT run_id FROM codex_review_intents`, tx.GetCodexReviewIntent},
			{`SELECT invocation_id FROM codex_review_outcomes`, tx.GetCodexReviewOutcome},
			{`SELECT invocation_id FROM codex_review_requests`, tx.GetCodexReviewRequest},
			{`SELECT source_run_id FROM codex_review_workspaces`, tx.GetCodexReviewWorkspace},
		} {
			keys, err := tx.storedKeys(ctx, table.query, "s")
			if err != nil {
				return err
			}
			for _, key := range keys {
				if err := discard(table.get(ctx, key.s[0])); err != nil {
					return err
				}
			}
		}
		return nil
	})},

	// Attention.
	{"attention item", []string{"attention_items", "attention_item_pr_references", "review_dispute_rows"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		rows, err := tx.ListAttentionItemRows(ctx)
		if err != nil {
			return err
		}
		// The list reports a refused row in place; the direct read decodes
		// each row again, so a refusal surfaces either way.
		for _, row := range rows {
			if row.Err != nil {
				return row.Err
			}
			if err := discard2(tx.GetAttentionItemSnapshot(ctx, row.ID)); err != nil {
				return err
			}
		}
		keys, err := tx.storedKeys(ctx, `SELECT item_id FROM attention_item_pr_references`, "s")
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := discard(tx.getAttentionItemPRReference(ctx, domain.ItemID(key.s[0]))); err != nil {
				return err
			}
		}
		return nil
	})},
	{"attention delivery", []string{"attention_deliveries"}, inRead(func(ctx context.Context, tx *ReadTx) error {
		return discard(tx.ListAttentionDeliveries(ctx))
	})},
	{"attention item view", []string{"attention_item_views"}, eachKey(`SELECT item_id, decision_surface_digest FROM attention_item_views`, "ss",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.GetAttentionItemView(ctx, domain.ItemID(k.s[0]), domain.Digest(k.s[1])))
		})},
	{"decision surface", []string{"attention_decision_surfaces"}, eachKey(`SELECT item_id FROM attention_decision_surfaces`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.DecisionSurface(ctx, domain.ItemID(k.s[0])))
		})},
	{"decision action surface", []string{"decision_action_surfaces"}, eachKey(`SELECT digest FROM decision_action_surfaces`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetDecisionActionSurface(ctx, domain.Digest(k.s[0])))
		})},
	{"recommendation source", []string{"attention_recommendation_sources"}, eachKey(`SELECT DISTINCT item_id FROM attention_recommendation_sources`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.ListRecommendationSources(ctx, domain.ItemID(k.s[0])))
		})},
	{"command", []string{"commands"}, eachKey(`SELECT command_id FROM commands`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			_, _, _, err := tx.getStoredCommandSnapshot(ctx, k.s[0])
			return err
		})},
	{"command receipt", []string{"client_command_receipts"}, eachKey(`SELECT command_id FROM client_command_receipts`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.GetCommandReceipt(ctx, k.s[0]))
		})},
	{"ready item PR binding", []string{"ready_item_pr_bindings"}, eachKey(`SELECT item_id FROM ready_item_pr_bindings`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetReadyItemPRBinding(ctx, domain.ItemID(k.s[0])))
		})},
	{"held item PR binding", []string{"held_item_pr_bindings"}, eachKey(`SELECT item_id FROM held_item_pr_bindings`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetHeldItemPRBinding(ctx, domain.ItemID(k.s[0])))
		})},

	// Effect proposals.
	{"effect proposal", []string{"effect_proposal_instances"}, eachKey(`SELECT instance_id FROM effect_proposal_instances`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetProposalInstance(ctx, domain.ProposalInstanceID(k.s[0])))
		})},
	{"effect proposal revision", []string{"effect_proposal_revisions"}, eachKey(`SELECT instance_id, content_digest FROM effect_proposal_revisions`, "ss",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			instance, err := tx.GetProposalInstance(ctx, domain.ProposalInstanceID(k.s[0]))
			if err != nil {
				return err
			}
			return discard2(tx.authenticatedProposalRevision(ctx, instance, domain.Digest(k.s[1])))
		})},
	{"effect proposal item", []string{"effect_proposal_items"}, eachKey(`SELECT item_id FROM effect_proposal_items`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard2(tx.ProposalForItem(ctx, domain.ItemID(k.s[0])))
		})},
	{"effect proposal snooze", []string{"effect_proposal_snoozes"}, eachKey(`SELECT DISTINCT i.item_id FROM effect_proposal_snoozes s JOIN effect_proposal_items i ON i.instance_id = s.instance_id`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.ProposalSnoozeUntil(ctx, domain.ItemID(k.s[0])))
		})},
	{"effect proposal decision", []string{"effect_proposal_decisions"}, eachKey(`SELECT instance_id FROM effect_proposal_decisions`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.ProposalDecisionForInstance(ctx, domain.ProposalInstanceID(k.s[0])))
		})},
	{"effect proposal closure approval", []string{"effect_proposal_policy_approvals"}, eachKey(`SELECT instance_id FROM effect_proposal_policy_approvals`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.ClosureApprovalForInstance(ctx, domain.ProposalInstanceID(k.s[0])))
		})},
	{"follow-up filing", []string{"follow_up_filing_intents", "follow_up_filing_attempts", "follow_up_filed_issues"}, walkFollowUpFiling},
	{"issue comment", []string{"issue_comment_intents", "issue_comment_attempts", "posted_issue_comments"}, walkIssueComments},
}

// walkFollowUpFiling reads each intent, which reconstructs its attempts, and
// then each filed issue by its own key: an intent's reconstruction only
// counts its filed issues, so a filed issue's body is decoded only here.
func walkFollowUpFiling(ctx context.Context, s *Store) error {
	intents := eachKey(`SELECT instance_id FROM follow_up_filing_intents`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetFollowUpFilingIntent(ctx, domain.ProposalInstanceID(k.s[0])))
		})
	if err := intents(ctx, s); err != nil {
		return err
	}
	filed := eachKey(`SELECT repository_id, issue_number FROM follow_up_filed_issues`, "nn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.FiledFollowUpIssue(ctx, k.n[0], int(k.n[1])))
		})
	return filed(ctx, s)
}

// walkIssueComments reads each intent, which reconstructs its attempts, and
// then each posted comment by its own key, as walkFollowUpFiling does.
func walkIssueComments(ctx context.Context, s *Store) error {
	intents := eachKey(`SELECT instance_id FROM issue_comment_intents`, "s",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.GetIssueCommentIntent(ctx, domain.ProposalInstanceID(k.s[0])))
		})
	if err := intents(ctx, s); err != nil {
		return err
	}
	posted := eachKey(`SELECT repository_id, comment_id FROM posted_issue_comments`, "nn",
		func(ctx context.Context, tx *ReadTx, k rowKey) error {
			return discard(tx.PostedIssueComment(ctx, k.n[0], k.n[1]))
		})
	return posted(ctx, s)
}
