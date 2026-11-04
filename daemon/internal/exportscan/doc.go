// Package exportscan is the §5.4 export scan (ward's check-7 hook): the last
// gate between an agent's verified output and the gauntlet. It answers one
// question — does this output carry credential material — and fails the
// handoff closed when it does, so no output reaches the importer unscanned.
//
// Scope, stated plainly: this is a high-signal pattern scan over the
// extracted export, not a proof of absence. It catches a credential copied
// or echoed into the workspace, which is the realistic Phase 1A.2 leak
// (the CLI's own auth store lives on a separate leased volume that never
// enters the export). It cannot catch material the agent encoded or split.
package exportscan
