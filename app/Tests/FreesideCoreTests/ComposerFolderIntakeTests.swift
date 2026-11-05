#if os(macOS)
    import Darwin
    import Foundation
    import FreesideAPI
    import FreesideModel
    import Testing

    @testable import FreesideCore

    /// A folder dropped on the Mac composer (#2591): the walk's skips and
    /// limits, and how the composer tells a folder inside the project's
    /// repository.
    @Suite struct ComposerFolderIntakeTests {
        private let fileManager = FileManager.default

        private func temporaryDirectory() throws -> URL {
            let url = fileManager.temporaryDirectory.appendingPathComponent(UUID().uuidString)
            try fileManager.createDirectory(at: url, withIntermediateDirectories: true)
            return url
        }

        private func write(_ text: String, to url: URL) throws {
            try fileManager.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            try Data(text.utf8).write(to: url)
        }

        /// A sparse file of `size` bytes, so a test passes a limit without
        /// writing the bytes.
        private func sparse(_ url: URL, size: Int) throws {
            try fileManager.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            #expect(fileManager.createFile(atPath: url.path, contents: nil))
            let handle = try FileHandle(forWritingTo: url)
            try handle.truncate(atOffset: UInt64(size))
            try handle.close()
        }

        private static let empty = TaskAttachmentDraft.Usage(count: 0, bytes: 0)

        private func walk(_ folder: URL, held: TaskAttachmentDraft.Usage = empty) -> ComposerAttachmentIntake.Item {
            ComposerFolderIntake.item(at: folder, repository: nil, held: held)
        }

        // MARK: Walk

        @Test func theWalkSkipsGitAndDSStoreAndLeavesOutLinks() throws {
            let root = try temporaryDirectory()
            defer { try? fileManager.removeItem(at: root) }
            let folder = root.appendingPathComponent("design")
            try write("top", to: folder.appendingPathComponent("brief.md"))
            try write("deep", to: folder.appendingPathComponent("screens/home/notes.txt"))
            try write("[core]", to: folder.appendingPathComponent(".git/config"))
            try write("gitdir: /elsewhere", to: folder.appendingPathComponent("vendor/.git"))
            try write("ds", to: folder.appendingPathComponent(".DS_Store"))
            try write("ds", to: folder.appendingPathComponent("screens/.DS_Store"))
            // A `.git` that is a link is skipped by name, not listed.
            try fileManager.createSymbolicLink(
                at: folder.appendingPathComponent("screens/.git"), withDestinationURL: root)
            try write("secret", to: root.appendingPathComponent("outside/secret.txt"))
            try fileManager.createSymbolicLink(
                at: folder.appendingPathComponent("screens/secret.txt"),
                withDestinationURL: root.appendingPathComponent("outside/secret.txt"))
            try fileManager.createSymbolicLink(
                at: folder.appendingPathComponent("outside"), withDestinationURL: root.appendingPathComponent("outside")
            )

            guard case .folder(let name, let files, let links) = walk(folder) else {
                Issue.record("design wasn't walked")
                return
            }
            #expect(name == "design")
            #expect(Set(files.map(\.path)) == ["brief.md", "screens/home/notes.txt"])
            #expect(files.first { $0.path == "brief.md" }?.bytes == Data("top".utf8))
            #expect(Set(links) == ["screens/secret.txt", "outside"])
            #expect(
                ComposerAttachmentIntake.skippedLinksSentence(
                    folder: "design", links: ["outside", "screens/secret.txt"])
                    == "design: 2 symbolic links weren't attached, such as outside.")
            #expect(
                ComposerAttachmentIntake.skippedLinksSentence(folder: "design", links: ["a"])
                    == "design: 1 symbolic link wasn't attached, such as a.")
            #expect(ComposerAttachmentIntake.skippedLinksSentence(folder: "design", links: []) == nil)
        }

        @Test func aFolderOverTheCountIsRefusedBeforeAnyFileIsRead() throws {
            let root = try temporaryDirectory()
            defer { try? fileManager.removeItem(at: root) }
            let folder = root.appendingPathComponent("logs")
            try write("a", to: folder.appendingPathComponent("a.log"))
            let unreadable = folder.appendingPathComponent("b.log")
            try write("b", to: unreadable)
            // A read would fail on this file, so a count refusal proves the
            // check ran first.
            try fileManager.setAttributes([.posixPermissions: 0], ofItemAtPath: unreadable.path)
            #expect(
                walk(folder, held: .init(count: TaskAttachmentLimits.maxCount - 1, bytes: 0))
                    == .refused(
                        "logs wasn't added: it would bring this task to more than 1,000 attachments, "
                            + "the most a task can have."))
            #expect(walk(folder) == .refused("logs wasn't added: logs/b.log couldn't be read."))
        }

        @Test func aFolderOverTheTotalIsRefusedFromItsSizes() throws {
            let root = try temporaryDirectory()
            defer { try? fileManager.removeItem(at: root) }
            let folder = root.appendingPathComponent("dump")
            for index in 0..<4 { try sparse(folder.appendingPathComponent("part\(index).bin"), size: 30 << 20) }
            #expect(
                walk(folder)
                    == .refused(
                        "dump wasn't added: it would bring this task's attachments to more than 100 MiB, "
                            + "the most they can total."))
        }

        @Test func aBatchOfFoldersCountsEachAgainstTheNext() throws {
            let root = try temporaryDirectory()
            defer { try? fileManager.removeItem(at: root) }
            var folders: [URL] = []
            for name in ["one", "two"] {
                let folder = root.appendingPathComponent(name)
                try sparse(folder.appendingPathComponent("part.bin"), size: 30 << 20)
                try sparse(folder.appendingPathComponent("more.bin"), size: 30 << 20)
                folders.append(folder)
            }
            let items = ComposerAttachmentIntake.files(at: folders, repository: nil, held: Self.empty)
            guard case .folder(_, let files, _) = items[0] else {
                Issue.record("the first folder wasn't read")
                return
            }
            #expect(files.count == 2)
            // 60 MiB read plus 60 MiB more passes the total, so the second
            // is refused before its files are read.
            #expect(
                items[1]
                    == .refused(
                        "two wasn't added: it would bring this task's attachments to more than 100 MiB, "
                            + "the most they can total."))
        }

        @Test func aFolderWithADirectoryThatCantBeListedIsRefusedWhole() throws {
            let root = try temporaryDirectory()
            let folder = root.appendingPathComponent("design")
            let locked = folder.appendingPathComponent("locked")
            defer {
                try? fileManager.setAttributes([.posixPermissions: 0o755], ofItemAtPath: locked.path)
                try? fileManager.removeItem(at: root)
            }
            try write("ok", to: folder.appendingPathComponent("ok/a.txt"))
            try write("hidden", to: locked.appendingPathComponent("b.txt"))
            try fileManager.setAttributes([.posixPermissions: 0], ofItemAtPath: locked.path)
            #expect(walk(folder) == .refused("design wasn't added: design/locked couldn't be read."))
        }

        @Test func aFolderWithAFileOverTheFileLimitIsRefusedWhole() throws {
            let root = try temporaryDirectory()
            defer { try? fileManager.removeItem(at: root) }
            let folder = root.appendingPathComponent("dump")
            try write("ok", to: folder.appendingPathComponent("ok.txt"))
            try sparse(folder.appendingPathComponent("sub/big.bin"), size: TaskAttachmentLimits.maxFileBytes + 1)
            #expect(
                walk(folder)
                    == .refused("dump wasn't added: dump/sub/big.bin is 32.1 MiB; a file can be at most 32 MiB."))
        }

        @Test func aFolderHoldingASpecialFileIsRefusedWhole() throws {
            let root = try temporaryDirectory()
            defer { try? fileManager.removeItem(at: root) }
            let folder = root.appendingPathComponent("run")
            try write("ok", to: folder.appendingPathComponent("ok.txt"))
            #expect(mkfifo(folder.appendingPathComponent("pipe").path, 0o600) == 0)
            #expect(walk(folder) == .refused("run wasn't added: run/pipe isn't a regular file."))
        }

        @Test func aFolderWithOnlySkippedEntriesHasNoFilesToAttach() throws {
            let root = try temporaryDirectory()
            defer { try? fileManager.removeItem(at: root) }
            let folder = root.appendingPathComponent("empty")
            try write("ds", to: folder.appendingPathComponent(".DS_Store"))
            guard case .folder(let name, let files, _) = walk(folder) else {
                Issue.record("empty wasn't walked")
                return
            }
            var draft = TaskAttachmentDraft()
            #expect(draft.addFolder(name: name, files: files) == .refused("empty has no files to attach."))
        }

        // MARK: Repository

        @Test func remoteURLsNameTheirRepository() {
            let cases: [(String, String?)] = [
                ("git@github.com:freeside-ai/freeside.git", "freeside-ai/freeside"),
                ("git@github.com:freeside-ai/freeside", "freeside-ai/freeside"),
                ("ssh://git@github.com/freeside-ai/freeside.git", "freeside-ai/freeside"),
                ("ssh://git@github.com:22/Freeside-AI/Freeside.git", "Freeside-AI/Freeside"),
                ("https://github.com/freeside-ai/freeside", "freeside-ai/freeside"),
                ("https://github.com/freeside-ai/freeside.git/", "freeside-ai/freeside"),
                ("https://github.com/freeside-ai", nil),
                ("/srv/git/freeside", nil),
            ]
            for (remote, name) in cases {
                #expect(ComposerFolderIntake.repositoryName(fromRemote: remote) == name, "\(remote)")
            }
        }

        @Test func onlyRemoteSectionsGiveURLs() {
            let config = """
                [core]
                \tbare = false
                [remote "origin"]
                \turl = git@github.com:freeside-ai/freeside.git
                \tfetch = +refs/heads/*:refs/remotes/origin/*
                [submodule "vendor"]
                \turl = https://github.com/other/vendor
                [remote "fork"]
                \tURL="https://github.com/someone/freeside"
                """
            #expect(
                ComposerFolderIntake.remoteURLs(inConfig: config)
                    == ["git@github.com:freeside-ai/freeside.git", "https://github.com/someone/freeside"])
        }

        /// A checkout at `root/name` whose config names `remote`.
        private func checkout(in root: URL, remote: String) throws -> URL {
            let checkout = root.appendingPathComponent("freeside")
            try write("[remote \"origin\"]\n\turl = \(remote)\n", to: checkout.appendingPathComponent(".git/config"))
            try write("handoff", to: checkout.appendingPathComponent("docs/design/handoff/brief.md"))
            return checkout
        }

        @Test func aFolderInsideTheProjectRepositoryIsItsPath() throws {
            let root = try temporaryDirectory()
            defer { try? fileManager.removeItem(at: root) }
            let checkout = try checkout(in: root, remote: "git@github.com:Freeside-AI/freeside.git")
            let handoff = checkout.appendingPathComponent("docs/design/handoff")
            #expect(
                ComposerFolderIntake.item(at: handoff, repository: "freeside-ai/freeside", held: Self.empty)
                    == .repositoryPath("docs/design/handoff/"))
            #expect(
                ComposerFolderIntake.item(at: checkout, repository: "freeside-ai/freeside", held: Self.empty)
                    == .refused("freeside is the project's repository, which the agent already has."))
            // Another project, or none known, uploads the folder.
            guard
                case .folder =
                    ComposerFolderIntake.item(at: handoff, repository: "other/repo", held: Self.empty)
            else {
                Issue.record("a folder in another repository wasn't walked")
                return
            }
            guard case .folder = walk(handoff) else {
                Issue.record("a folder with no known repository wasn't walked")
                return
            }
        }

        @Test func aLinkedWorktreeFindsTheSharedConfig() throws {
            let root = try temporaryDirectory()
            defer { try? fileManager.removeItem(at: root) }
            let main = try checkout(in: root, remote: "https://github.com/freeside-ai/freeside")
            let gitDirectory = main.appendingPathComponent(".git/worktrees/feature")
            try write("../..\n", to: gitDirectory.appendingPathComponent("commondir"))
            let worktree = root.appendingPathComponent("feature")
            try write("gitdir: \(gitDirectory.path)\n", to: worktree.appendingPathComponent(".git"))
            try write("x", to: worktree.appendingPathComponent("assets/logo.svg"))
            #expect(
                ComposerFolderIntake.item(
                    at: worktree.appendingPathComponent("assets"), repository: "freeside-ai/freeside",
                    held: Self.empty)
                    == .repositoryPath("assets/"))
        }

        @Test func aFolderOutsideAnyCheckoutHasNoCheckout() throws {
            let root = try temporaryDirectory()
            defer { try? fileManager.removeItem(at: root) }
            #expect(ComposerFolderIntake.enclosingCheckout(of: root.resolvingSymlinksInPath()) == nil)
        }
    }
#endif
