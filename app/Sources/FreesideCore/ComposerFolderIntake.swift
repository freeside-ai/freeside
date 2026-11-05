#if os(macOS)
    import Foundation
    import FreesideModel
    import UniformTypeIdentifiers

    /// Reads a folder the operator dropped, pasted, or picked into the Mac
    /// composer (#2591). The agent runs in a container that can't see host
    /// paths, so a folder's files are uploaded and the daemon recreates the
    /// tree, unless the folder sits inside the project's own repository,
    /// which the agent already has: then the composer inserts its path.
    ///
    /// The Mac app isn't sandboxed, so it can read the folder's ancestors to
    /// find the enclosing checkout. A sandboxed app would need another way.
    enum ComposerFolderIntake {
        /// The folder at `url` as a composer item. `repository` is the
        /// project's `owner/repo`, nil when the client doesn't know it;
        /// `held` is what the composer and the batch being read already
        /// take, so a folder that would pass the task's count or total is
        /// refused before any file is read.
        static func item(
            at url: URL, repository: String?, held: TaskAttachmentDraft.Usage
        ) -> ComposerAttachmentIntake.Item {
            let folder = url.standardizedFileURL.resolvingSymlinksInPath()
            let name = url.lastPathComponent
            if let repository, let checkout = enclosingCheckout(of: folder),
                checkoutMatches(checkout, repository: repository)
            {
                let relative = relativePath(of: folder, in: checkout)
                return relative.isEmpty
                    ? .refused("\(name) is the project's repository, which the agent already has.")
                    : .repositoryPath(relative + "/")
            }
            return walk(folder, name: name, held: held)
        }

        // MARK: Walk

        /// Reads every regular file under `folder`, never following a
        /// symlink, so nothing outside the folder is read. `.git` at any
        /// depth and `.DS_Store` are skipped; a symlink is left out and
        /// named. Any other kind of entry, a directory that can't be listed,
        /// a file the limits refuse, or a file that can't be read refuses
        /// the whole folder: a tree missing a file would mislead the agent.
        /// The walk stops as soon as the folder passes the task's count or
        /// total, so dropping a huge tree doesn't enumerate all of it.
        static func walk(_ folder: URL, name: String, held: TaskAttachmentDraft.Usage) -> ComposerAttachmentIntake.Item
        {
            let fileManager = FileManager.default
            // The enumerator silently skips a directory it can't open, so
            // each directory is listed once to catch that.
            func unlistable(_ path: String) -> Bool {
                (try? fileManager.contentsOfDirectory(atPath: path)) == nil
            }
            guard !unlistable(folder.path), let enumerator = fileManager.enumerator(atPath: folder.path) else {
                return .refused("\(name) couldn't be read.")
            }
            var found: [String] = []
            var taken = TaskAttachmentDraft.Usage(count: 0, bytes: 0)
            var links: [String] = []
            while let path = enumerator.nextObject() as? String {
                let entry = (path as NSString).lastPathComponent
                // `fileAttributes` describes the entry itself, not a link's
                // target, and the enumerator never descends through a link.
                let type = enumerator.fileAttributes?[.type] as? FileAttributeType
                if entry == ".git" || entry == ".DS_Store" {
                    if type == .typeDirectory { enumerator.skipDescendants() }
                    continue
                }
                switch type {
                case .typeSymbolicLink:
                    links.append(path)
                case .typeDirectory:
                    if unlistable(folder.appendingPathComponent(path).path) {
                        return .refused("\(name) wasn't added: \(name)/\(path) couldn't be read.")
                    }
                case .typeRegular:
                    let size = (enumerator.fileAttributes?[.size] as? NSNumber)?.intValue ?? 0
                    if let refusal = TaskAttachmentDraft.oversizedFileRefusal(name: "\(name)/\(path)", sizeBytes: size)
                    {
                        return .refused("\(name) wasn't added: \(refusal)")
                    }
                    found.append(path)
                    taken.count += 1
                    taken.bytes += size
                    if let refusal = TaskAttachmentDraft.folderRefusal(name: name, adding: taken, to: held) {
                        return .refused(refusal)
                    }
                default:
                    return .refused("\(name) wasn't added: \(name)/\(path) isn't a regular file.")
                }
            }
            var files: [FolderFile] = []
            files.reserveCapacity(found.count)
            for path in found {
                let url = folder.appendingPathComponent(path)
                guard let bytes = try? Data(contentsOf: url) else {
                    return .refused("\(name) wasn't added: \(name)/\(path) couldn't be read.")
                }
                files.append(
                    FolderFile(
                        path: path, claimedMediaType: ComposerAttachmentIntake.claim(for: url, nil), bytes: bytes))
            }
            return .folder(name: name, files: files, skippedLinks: links)
        }

        // MARK: Repository

        /// The folder itself or its closest ancestor holding `.git`, a
        /// directory in a plain checkout or a file in a linked worktree or
        /// submodule.
        static func enclosingCheckout(of folder: URL) -> URL? {
            // Trims path components rather than calling
            // `deletingLastPathComponent`, which turns "/" into "/.." and
            // never reaches a fixed point.
            var components = folder.pathComponents
            while !components.isEmpty {
                let candidate = URL(fileURLWithPath: NSString.path(withComponents: components), isDirectory: true)
                if FileManager.default.fileExists(atPath: candidate.appendingPathComponent(".git").path) {
                    return candidate
                }
                components.removeLast()
            }
            return nil
        }

        /// Whether any of the checkout's remotes names `repository`,
        /// ignoring case.
        static func checkoutMatches(_ checkout: URL, repository: String) -> Bool {
            guard let config = configFile(ofCheckout: checkout),
                let text = try? String(contentsOf: config, encoding: .utf8)
            else { return false }
            let wanted = repository.lowercased()
            return remoteURLs(inConfig: text).contains { repositoryName(fromRemote: $0)?.lowercased() == wanted }
        }

        /// The checkout's git config. A `.git` file names the real git
        /// directory (`gitdir: <path>`); a linked worktree's git directory
        /// holds a `commondir` file naming the shared one, where the config
        /// lives.
        static func configFile(ofCheckout checkout: URL) -> URL? {
            let dotGit = checkout.appendingPathComponent(".git")
            var isDirectory: ObjCBool = false
            guard FileManager.default.fileExists(atPath: dotGit.path, isDirectory: &isDirectory) else { return nil }
            var gitDirectory = dotGit
            if !isDirectory.boolValue {
                guard let text = try? String(contentsOf: dotGit, encoding: .utf8),
                    let line = text.split(whereSeparator: \.isNewline).first,
                    line.hasPrefix("gitdir:")
                else { return nil }
                let path = line.dropFirst("gitdir:".count).trimmingCharacters(in: .whitespaces)
                gitDirectory = resolve(path, against: checkout)
            }
            if let common = try? String(contentsOf: gitDirectory.appendingPathComponent("commondir"), encoding: .utf8) {
                gitDirectory = resolve(common.trimmingCharacters(in: .whitespacesAndNewlines), against: gitDirectory)
            }
            return gitDirectory.appendingPathComponent("config")
        }

        private static func resolve(_ path: String, against base: URL) -> URL {
            (path.hasPrefix("/") ? URL(fileURLWithPath: path) : base.appendingPathComponent(path))
                .standardizedFileURL
        }

        /// The `url` values of every `[remote "…"]` section in a git config.
        static func remoteURLs(inConfig text: String) -> [String] {
            var urls: [String] = []
            var inRemote = false
            for raw in text.split(whereSeparator: \.isNewline) {
                let line = raw.trimmingCharacters(in: .whitespaces)
                if line.hasPrefix("[") {
                    inRemote = line.lowercased().hasPrefix("[remote ")
                    continue
                }
                guard inRemote, let equals = line.firstIndex(of: "=") else { continue }
                let key = line[..<equals].trimmingCharacters(in: .whitespaces).lowercased()
                guard key == "url" else { continue }
                var value = line[line.index(after: equals)...].trimmingCharacters(in: .whitespaces)
                if value.count >= 2, value.hasPrefix("\""), value.hasSuffix("\"") {
                    value = String(value.dropFirst().dropLast())
                }
                if !value.isEmpty { urls.append(value) }
            }
            return urls
        }

        /// `owner/repo` from a remote URL: `git@host:owner/repo.git`,
        /// `ssh://git@host/owner/repo.git`, or `https://host/owner/repo`.
        /// Nil when the path has fewer than two parts.
        static func repositoryName(fromRemote remote: String) -> String? {
            let path: Substring
            if let scheme = remote.range(of: "://") {
                let rest = remote[scheme.upperBound...]
                guard let slash = rest.firstIndex(of: "/") else { return nil }
                path = rest[slash...]
            } else if let colon = remote.firstIndex(of: ":") {
                path = remote[remote.index(after: colon)...]
            } else {
                return nil
            }
            let parts = path.split(separator: "/")
            guard parts.count >= 2 else { return nil }
            var repo = parts[parts.count - 1]
            if repo.hasSuffix(".git") { repo = repo.dropLast(4) }
            guard !repo.isEmpty else { return nil }
            return "\(parts[parts.count - 2])/\(repo)"
        }

        /// `folder`'s path below `checkout`, "" for the checkout itself.
        static func relativePath(of folder: URL, in checkout: URL) -> String {
            folder.pathComponents.dropFirst(checkout.pathComponents.count).joined(separator: "/")
        }
    }
#endif
