import ObjectiveC

// The loader decodes images with ImageIO, so it stays with the views while
// InboxStore builds on Linux. The store still owns the one loader: it is
// attached to the store object, so it lives and dies with it. #2172 injects
// the decoder, after which the loader can move back into the store.
private nonisolated(unsafe) var attachmentLoaderKey: UInt8 = 0

extension InboxStore {
    /// The card-shared attachment loader over the same client: digests
    /// are content-addressed, so one memory-only table serves every
    /// card instance (plan §5.14 keeps attachment bytes out of the
    /// disk cache).
    public var attachments: AttachmentLoader {
        if let loader = objc_getAssociatedObject(self, &attachmentLoaderKey) as? AttachmentLoader {
            return loader
        }
        let loader = AttachmentLoader(client: client)
        objc_setAssociatedObject(self, &attachmentLoaderKey, loader, .OBJC_ASSOCIATION_RETAIN_NONATOMIC)
        return loader
    }
}
