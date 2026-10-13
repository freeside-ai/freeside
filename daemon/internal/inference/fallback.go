package inference

// FallbackReason names a daemon-authored cause, never provider or input text.
type FallbackReason string

const (
	FallbackFieldPolicyRejected         FallbackReason = "field_policy_rejected"
	FallbackFieldNotUTF8                FallbackReason = "field_not_utf8"
	FallbackFieldAllowlistIncomplete    FallbackReason = "field_allowlist_incomplete"
	FallbackRoleSourceAbsent            FallbackReason = "role_source_absent"
	FallbackRoleOff                     FallbackReason = "role_off"
	FallbackRoleUnbound                 FallbackReason = "role_unbound"
	FallbackInputTooLarge               FallbackReason = "input_too_large"
	FallbackDriverAbsent                FallbackReason = "driver_absent"
	FallbackCallInFlight                FallbackReason = "call_in_flight"
	FallbackBudgetExhausted             FallbackReason = "budget_exhausted"
	FallbackLedgerUnavailable           FallbackReason = "ledger_unavailable"
	FallbackDriverFailed                FallbackReason = "driver_failed"
	FallbackDeadlineExceeded            FallbackReason = "deadline_exceeded"
	FallbackResponseExceededContract    FallbackReason = "response_exceeded_contract"
	FallbackCallRecordPersistenceFailed FallbackReason = "call_record_persistence_failed"
	FallbackOutputSchemaRejected        FallbackReason = "output_schema_rejected"
	FallbackAuditPersistenceFailed      FallbackReason = "audit_persistence_failed"
)

var AllFallbackReasons = []FallbackReason{
	FallbackFieldPolicyRejected, FallbackFieldNotUTF8, FallbackFieldAllowlistIncomplete,
	FallbackRoleSourceAbsent, FallbackRoleOff, FallbackRoleUnbound, FallbackInputTooLarge,
	FallbackDriverAbsent, FallbackCallInFlight, FallbackBudgetExhausted, FallbackLedgerUnavailable,
	FallbackDriverFailed, FallbackDeadlineExceeded, FallbackResponseExceededContract,
	FallbackCallRecordPersistenceFailed, FallbackOutputSchemaRejected, FallbackAuditPersistenceFailed,
}

func (r FallbackReason) valid() bool {
	switch r {
	case FallbackFieldPolicyRejected, FallbackFieldNotUTF8, FallbackFieldAllowlistIncomplete,
		FallbackRoleSourceAbsent, FallbackRoleOff, FallbackRoleUnbound, FallbackInputTooLarge,
		FallbackDriverAbsent, FallbackCallInFlight, FallbackBudgetExhausted, FallbackLedgerUnavailable,
		FallbackDriverFailed, FallbackDeadlineExceeded, FallbackResponseExceededContract,
		FallbackCallRecordPersistenceFailed, FallbackOutputSchemaRejected, FallbackAuditPersistenceFailed:
		return true
	default:
		return false
	}
}
