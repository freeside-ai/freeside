#if os(macOS)
    import AppKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore

    @Suite @MainActor struct TaskLifecycleVisualTests {
        @Test func terminalTaskTimelineHeaders() throws {
            _ = FreesideFont.registration
            let active = try #require(TaskFixtures.defaultTasks().first { $0.task.id == TaskFixtures.retryTaskID })
            let timeline = try #require(TaskFixtures.defaultTimelines().first { $0.task_id == active.task.id })
            let coordinator = SyncCoordinator(client: APIClientFactory.mock(), cache: InMemoryCacheStore())
            let terminals = [TaskFixtures.confirmedStopped(active), TaskFixtures.explicitlyAbandoned(active)]
            for width in [820.0, 390.0] {
                for scheme in [ColorScheme.light, .dark] {
                    let view = VStack(spacing: 0) {
                        ForEach(terminals.indices, id: \.self) { index in
                            TaskTimelineView(coordinator: coordinator, snapshot: terminals[index], onOpenRun: { _ in })
                                .screenshotContent(timeline)
                                .frame(width: width, height: 220, alignment: .topLeading)
                                .clipped()
                        }
                    }
                    .environment(\.dynamicTypeSize, .large)
                    .environment(\.locale, Locale(identifier: "en_US_POSIX"))
                    .environment(\.timeZone, TimeZone(identifier: "UTC")!)
                    .background(Color.ground)
                    .environment(\.colorScheme, scheme)
                    let renderer = ImageRenderer(content: view)
                    renderer.scale = 2
                    let image = try #require(renderer.cgImage)
                    #expect(image.width == Int(width * 2))
                    if let output = ProcessInfo.processInfo.environment["FREESIDE_LIFECYCLE_CAPTURE"] {
                        let dir = URL(fileURLWithPath: output)
                        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
                        let png = try #require(
                            NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:]))
                        try png.write(to: dir.appendingPathComponent("timeline-\(Int(width))-\(scheme).png"))
                    }
                }
            }
        }

        @Test func terminalTaskFilters() throws {
            _ = FreesideFont.registration
            var tasks = TaskFixtures.defaultTasks().filter { $0.task.lifecycle == .active }
            #expect(tasks.count == 3)
            tasks[0] = TaskFixtures.confirmedStopped(tasks[0])
            tasks[1] = TaskFixtures.explicitlyAbandoned(tasks[1])
            #expect(TaskListFilter(scope: .active).rows(in: tasks).count == 1)
            #expect(TaskListFilter(scope: .finished).rows(in: tasks).count == 2)
            for legacyProjection in [true, false] {
                var visible = tasks
                if legacyProjection {
                    // Reconstruct the old wire projection over identical
                    // cancellation/abandonment facts: newest runs remain active.
                    visible[0].task.lifecycle = .active
                    visible[1].task.lifecycle = .active
                }
                let count = OperationalSummary(openSnapshots: [], tasks: visible, freshness: .fresh).activeTaskCount
                #expect(count == (legacyProjection ? 3 : 1))
                for width in [820.0, 390.0] {
                    for scheme in [ColorScheme.light, .dark] {
                        let view = VStack(alignment: .leading, spacing: 12) {
                            Text(
                                "Active tasks: \(count)"
                            )
                            .font(.headline)
                            .padding(.horizontal)
                            TasksListView(
                                tasks: visible, runs: RunFixtures.defaultRuns(), schedules: [],
                                selection: .constant(nil), initialScope: .active
                            ).screenshotContent(now: RunFixtures.screenshotInstant)
                        }
                        .environment(\.dynamicTypeSize, .large)
                        .environment(\.locale, Locale(identifier: "en_US_POSIX"))
                        .environment(\.timeZone, TimeZone(identifier: "UTC")!)
                        .padding(.top, 12)
                        .frame(width: width, height: 500, alignment: .top)
                        .background(Color.ground)
                        .environment(\.colorScheme, scheme)
                        let renderer = ImageRenderer(content: view)
                        renderer.scale = 2
                        let image = try #require(renderer.cgImage)
                        #expect(image.width == Int(width * 2))
                        if let output = ProcessInfo.processInfo.environment["FREESIDE_LIFECYCLE_CAPTURE"] {
                            let dir = URL(fileURLWithPath: output)
                            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
                            let png = try #require(
                                NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:]))
                            let prefix = legacyProjection ? "before" : "after"
                            try png.write(to: dir.appendingPathComponent("\(prefix)-tasks-\(Int(width))-\(scheme).png"))
                        }
                    }
                }
            }
        }
    }
#endif
