#if os(macOS)
    import Foundation

    /// Maps every screenshot matrix surface to its digest family and flat
    /// resource file (`ScreenshotDigests-<family>.json`).
    ///
    /// To classify a new surface, add its dash-delimited prefix to the matching
    /// family in `ScreenshotFamily.all` below (longest matching prefix wins).
    struct ScreenshotFamily: Equatable, Hashable, Sendable {
        let name: String
        let prefixes: [String]

        var resourceName: String { "\(Self.resourcePrefix)\(name)" }
        var fileName: String { "\(resourceName).json" }

        static let resourcePrefix = "ScreenshotDigests-"

        static let all: [ScreenshotFamily] = [
            ScreenshotFamily(
                name: "decision-spec-approval",
                prefixes: ["decision-spec_approval"]),
            ScreenshotFamily(
                name: "decision-review-dispute",
                prefixes: ["decision-review_dispute"]),
            ScreenshotFamily(
                name: "decision-final-review",
                prefixes: ["decision-ready", "decision-ready_for_final_review"]),
            ScreenshotFamily(
                name: "decision-review-findings",
                prefixes: [
                    "decision-finding_adjudication",
                    "decision-review_diminishing_returns",
                    "decision-review_configuration",
                    "decision-review_contradiction",
                ]),
            ScreenshotFamily(
                name: "decision-proposals",
                prefixes: [
                    "decision-agent_question",
                    "decision-effect_proposal",
                    "effect-proposal",
                    "decision-task_proposal",
                    "task-proposal",
                ]),
            ScreenshotFamily(
                name: "decision-status",
                prefixes: [
                    "decision-execution_failure",
                    "decision-system_health",
                    "decision-publish_blocked",
                    "decision-stall",
                    "decision-retryable",
                    "decision-feedback",
                    "decision-banner",
                    "decision-newer_type",
                ]),
            ScreenshotFamily(
                name: "task-page",
                prefixes: ["task-page"]),
            ScreenshotFamily(
                name: "task-holds-stops",
                prefixes: ["task-hold", "task-holds", "task-stop"]),
            ScreenshotFamily(
                name: "task-stages",
                prefixes: [
                    "task-stage",
                    "task-history",
                    "task-progress",
                    "task-approval",
                    "task-published",
                    "task-external",
                    "task-legacy",
                    "task-review",
                    "task-style",
                ]),
            ScreenshotFamily(
                name: "run-review",
                prefixes: ["run-review", "review-evidence", "review-output"]),
            ScreenshotFamily(
                name: "run-timeline",
                prefixes: ["run-timeline", "agent-transcript"]),
            ScreenshotFamily(
                name: "inbox-list",
                prefixes: [
                    "unified-list",
                    "unattended-stopped",
                    "tasks",
                    "inbox",
                    "item-scope",
                    "freshness-banner",
                    "project-filter",
                    "detail-column",
                    "standing-notices",
                    "unavailable-state",
                ]),
            ScreenshotFamily(
                name: "composers",
                prefixes: [
                    "new-task",
                    "message-composer",
                    "discuss-composer",
                    "conversation",
                    "answer-retry",
                    "consequence-sheet",
                ]),
            ScreenshotFamily(
                name: "readers-controls",
                prefixes: [
                    "task-prompt",
                    "task-specification",
                    "attachment",
                    "card-style",
                    "choice-list",
                    "segmented-control",
                ]),
            ScreenshotFamily(
                name: "settings-routing",
                prefixes: ["routing", "workflow", "role-picker"]),
            ScreenshotFamily(
                name: "settings-usage",
                prefixes: ["usage", "work-status", "system-health"]),
            ScreenshotFamily(
                name: "settings-general",
                prefixes: [
                    "settings",
                    "devices",
                    "notifications",
                    "daemon",
                    "general-phone",
                    "pairing",
                    "pair-device",
                    "menu-panel",
                    "compatibility-refusal",
                    "revoked-pane",
                ]),
        ]

        static let byName: [String: ScreenshotFamily] = Dictionary(
            uniqueKeysWithValues: all.map { ($0.name, $0) })

        /// Resolve a surface name or manifest key against `families` using
        /// longest dash-delimited prefix match.
        static func family(
            ofSurface surfaceName: String,
            in families: [ScreenshotFamily] = all
        ) throws -> ScreenshotFamily {
            var bestLength = -1
            var matched: [ScreenshotFamily] = []
            for family in families {
                var familyBest = -1
                for prefix in family.prefixes
                where surfaceName == prefix || surfaceName.hasPrefix("\(prefix)-") {
                    familyBest = max(familyBest, prefix.count)
                }
                guard familyBest >= 0 else { continue }
                if familyBest > bestLength {
                    bestLength = familyBest
                    matched = [family]
                } else if familyBest == bestLength {
                    matched.append(family)
                }
            }
            guard let winner = matched.first else {
                throw ScreenshotFamilyError.unclassifiedSurface(surfaceName)
            }
            guard matched.count == 1 else {
                throw ScreenshotFamilyError.ambiguousSurface(
                    surfaceName, families: matched.map(\.name))
            }
            return winner
        }

        /// Matrix keys append `-<size>` after a dash, so the dash-delimited
        /// prefix rule resolves a full key identically to its surface name.
        static func family(
            ofKey key: String,
            in families: [ScreenshotFamily] = all
        ) throws -> ScreenshotFamily {
            try family(ofSurface: key, in: families)
        }

        /// Parse `FREESIDE_SCREENSHOT_FAMILY=<family>[,<family>...]`.
        /// Returns `nil` when unset; rejects empty, malformed, or unknown names.
        static func parseSelection(
            _ raw: String?,
            in families: [ScreenshotFamily] = all
        ) throws -> [ScreenshotFamily]? {
            guard let raw else { return nil }
            guard !raw.isEmpty else { throw ScreenshotFamilyError.invalidFamilySelection(raw) }
            let lookup = Dictionary(uniqueKeysWithValues: families.map { ($0.name, $0) })
            var selected: [ScreenshotFamily] = []
            var seen: Set<ScreenshotFamily> = []
            for part in raw.split(separator: ",", omittingEmptySubsequences: false) {
                let name = String(part)
                guard !name.isEmpty, let family = lookup[name] else {
                    throw ScreenshotFamilyError.invalidFamilySelection(raw)
                }
                if seen.insert(family).inserted {
                    selected.append(family)
                }
            }
            return selected
        }

        /// Reject any `ScreenshotDigests-*.json` resource file not named by
        /// `families`.
        static func validateResourceFileNames(
            _ fileNames: [String],
            in families: [ScreenshotFamily] = all
        ) throws {
            let known = Set(families.map(\.fileName))
            let unknown = fileNames.filter {
                $0.hasPrefix(resourcePrefix) && $0.hasSuffix(".json") && !known.contains($0)
            }.sorted()
            if let first = unknown.first {
                throw ScreenshotFamilyError.unknownDigestFile(first)
            }
        }

        /// Merge decoded per-family manifests, rejecting misplaced or duplicate
        /// keys.
        static func mergeDecodedFiles(
            _ decoded: [(family: ScreenshotFamily, digests: [String: String])],
            in families: [ScreenshotFamily] = all
        ) throws -> [String: String] {
            var merged: [String: String] = [:]
            var sourceByKey: [String: String] = [:]
            for entry in decoded {
                for (key, digest) in entry.digests {
                    if let previous = sourceByKey[key] {
                        throw ScreenshotFamilyError.duplicateDigestKey(
                            key: key, files: [previous, entry.family.fileName])
                    }
                    let expectedFamily = try family(ofKey: key, in: families)
                    if expectedFamily != entry.family {
                        throw ScreenshotFamilyError.misplacedDigestKey(
                            key: key,
                            expectedFamily: expectedFamily.name,
                            actualFile: entry.family.fileName)
                    }
                    sourceByKey[key] = entry.family.fileName
                    merged[key] = digest
                }
            }
            return merged
        }

        /// Partition newly captured digests into their owning families.
        static func partition(
            _ digests: [String: String],
            allowedFamilies: Set<ScreenshotFamily>,
            in families: [ScreenshotFamily] = all
        ) throws -> [ScreenshotFamily: [String: String]] {
            var grouped: [ScreenshotFamily: [String: String]] = [:]
            for family in allowedFamilies {
                grouped[family] = [:]
            }
            for (key, digest) in digests {
                let owner = try family(ofKey: key, in: families)
                guard allowedFamilies.contains(owner) else {
                    throw ScreenshotFamilyError.misplacedDigestKey(
                        key: key,
                        expectedFamily: owner.name,
                        actualFile: "selected-families")
                }
                grouped[owner, default: [:]][key] = digest
            }
            return grouped
        }

        struct StaleOverride: Equatable, Sendable {
            let key: String
            let operatingSystems: [String]
        }

        /// Identify keys with entries in `ScreenshotDigestOverrides.json` whose
        /// recorded baseline changed (or whose prior family file could not be
        /// decoded, as after a merge conflict).
        static func staleOverrides(
            recordedByFamily: [ScreenshotFamily: [String: String]],
            priorByFamily: [ScreenshotFamily: [String: String]?],
            overrides: [String: [String: String]]
        ) -> [StaleOverride] {
            var osByKey: [String: [String]] = [:]
            for (osKey, entries) in overrides {
                for key in entries.keys {
                    osByKey[key, default: []].append(osKey)
                }
            }
            guard !osByKey.isEmpty else { return [] }

            var changed: Set<String> = []
            for (family, recorded) in recordedByFamily {
                if let prior = priorByFamily[family] ?? nil {
                    for (key, digest) in recorded where prior[key] != digest {
                        if osByKey[key] != nil { changed.insert(key) }
                    }
                    for key in prior.keys where recorded[key] == nil {
                        if osByKey[key] != nil { changed.insert(key) }
                    }
                } else {
                    // Prior file was unreadable (for example, conflict markers),
                    // so any overridden key in this family may now be stale.
                    for key in recorded.keys where osByKey[key] != nil {
                        changed.insert(key)
                    }
                }
            }
            return changed.sorted().map { key in
                StaleOverride(key: key, operatingSystems: (osByKey[key] ?? []).sorted())
            }
        }
    }

    enum ScreenshotFamilyError: Error, CustomStringConvertible, Equatable {
        case ambiguousSurface(String, families: [String])
        case duplicateDigestKey(key: String, files: [String])
        case invalidFamilySelection(String)
        case misplacedDigestKey(key: String, expectedFamily: String, actualFile: String)
        case unclassifiedSurface(String)
        case unknownDigestFile(String)

        var description: String {
            switch self {
            case .ambiguousSurface(let surface, let families):
                let list = families.joined(separator: ", ")
                return "Screenshot surface or key '\(surface)' matches multiple families (\(list)); "
                    + "disambiguate its prefix in ScreenshotFamily.all (ScreenshotFamilies.swift)."
            case .duplicateDigestKey(let key, let files):
                let list = files.joined(separator: ", ")
                return "Screenshot digest key '\(key)' appears in multiple family files: \(list)."
            case .invalidFamilySelection(let raw):
                let names = ScreenshotFamily.all.map(\.name).joined(separator: ", ")
                return "Invalid FREESIDE_SCREENSHOT_FAMILY selection '\(raw)'. "
                    + "Expected comma-separated names from: \(names)."
            case .misplacedDigestKey(let key, let expectedFamily, let actualFile):
                return "Screenshot digest key '\(key)' belongs to family '\(expectedFamily)' "
                    + "but was found in \(actualFile)."
            case .unclassifiedSurface(let surface):
                return "Screenshot surface or key '\(surface)' matches no family; "
                    + "add its prefix to ScreenshotFamily.all in app/Tests/FreesideCoreTests/ScreenshotFamilies.swift."
            case .unknownDigestFile(let fileName):
                return "Resource file '\(fileName)' matches no family in "
                    + "ScreenshotFamily.all (ScreenshotFamilies.swift)."
            }
        }
    }
#endif
