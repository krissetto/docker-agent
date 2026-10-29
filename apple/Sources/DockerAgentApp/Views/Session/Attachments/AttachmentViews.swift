import SwiftUI
import PhotosUI
import UniformTypeIdentifiers
import QuickLook
import AgentKit
import ImageIO

struct AttachmentChips: View {
    let attachments: [SessionDocumentAttachment]
    let usedBytes: Int
    let remove: (SessionDocumentAttachment) -> Void

    private let budget = SessionSendPayload.maximumEncodedRequestBytes
    private var overBudget: Bool { usedBytes > budget }

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            ScrollView(.horizontal) {
                HStack(spacing: Metrics.s) {
                    ForEach(attachments) { attachment in
                        AttachmentChip(attachment: attachment) { remove(attachment) }
                    }
                }
                .padding(.vertical, Metrics.xxs)
            }
            .scrollIndicators(.hidden)
            Text("\(usedBytes.formatted(.byteCount(style: .file))) of \(budget.formatted(.byteCount(style: .file))) app limit per message")
                .font(.caption2)
                .foregroundStyle(overBudget ? .red : .secondary)
        }
    }
}

private struct AttachmentChip: View {
    let attachment: SessionDocumentAttachment
    let remove: () -> Void

    var body: some View {
        HStack(spacing: Metrics.s) {
            preview
                .frame(width: 32, height: 32)
                .clipShape(.rect(cornerRadius: 6, style: .continuous))
            VStack(alignment: .leading, spacing: 0) {
                Text(attachment.name)
                    .font(.caption.weight(.medium))
                    .lineLimit(1)
                    .truncationMode(.middle)
                Text(attachment.byteCount.formatted(.byteCount(style: .file)))
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            .frame(maxWidth: 160, alignment: .leading)
            Button(action: remove) {
                Image(systemName: "xmark.circle.fill").foregroundStyle(.secondary)
            }
            .buttonStyle(.borderless)
            .accessibilityLabel("Remove \(attachment.name)")
        }
        .padding(Metrics.xs)
        .padding(.trailing, Metrics.xs)
        .background(.fill.tertiary, in: .rect(cornerRadius: Metrics.chipRadius, style: .continuous))
        .accessibilityElement(children: .combine)
    }

    @ViewBuilder
    private var preview: some View {
        if MediaKind(mimeType: attachment.mimeType) == .image,
           let encoded = attachment.part.document.source.inlineData,
           encoded.utf8.count <= SessionDocumentAttachment.maximumDocumentBytes * 4 / 3 + 4,
           let data = Data(base64Encoded: encoded),
           let image = PlatformImage.make(data, maxPixelSize: 96) {
            image.resizable().scaledToFill()
        } else {
            ZStack {
                Rectangle().fill(.fill.secondary)
                Image(systemName: MediaKind(mimeType: attachment.mimeType).symbol).foregroundStyle(.secondary)
            }
        }
    }
}

/// Paperclip menu: Photos and Files (paste/drop are handled by the composer).
/// The composer's one leading menu: everything that adds to a message lives here, so the field keeps its width.
struct AttachmentMenu: View {
    let allowedTypes: [UTType]
    let addFiles: ([URL]) -> Void
    let addPhoto: (Data, _ name: String, _ mimeType: String) -> Void
    var openPrompts: (() -> Void)? = nil

    @State private var importing = false
    @State private var photoItems: [PhotosPickerItem] = []
    @State private var pickingPhotos = false

    var body: some View {
        Menu {
            Button("Photos…", systemImage: "photo.on.rectangle") { pickingPhotos = true }
            Button("Files…", systemImage: "doc") { importing = true }
            if let openPrompts {
                Divider()
                Button("Prompts & Skills…", systemImage: "sparkles", action: openPrompts)
            }
        } label: {
            Image(systemName: "plus.circle.fill")
                .font(.title2)
                .symbolRenderingMode(.hierarchical)
                .frame(width: Metrics.target, height: Metrics.target)
                .contentShape(.rect)
        }
        .menuStyle(.button)
        .buttonStyle(.borderless)
        .menuIndicator(.hidden)
        .fixedSize()
        .accessibilityLabel(openPrompts == nil ? "Attach" : "Attach or insert a prompt")
        .fileImporter(isPresented: $importing, allowedContentTypes: allowedTypes, allowsMultipleSelection: true) { result in
            if case .success(let urls) = result { addFiles(urls) }
        }
        .photosPicker(isPresented: $pickingPhotos, selection: $photoItems, maxSelectionCount: 4, matching: .images)
        .onChange(of: photoItems) { _, items in
            guard !items.isEmpty else { return }
            photoItems = []
            for item in items {
                Task {
                    guard let data = try? await item.loadTransferable(type: Data.self) else { return }
                    let type = item.supportedContentTypes.first { $0.conforms(to: .image) } ?? .jpeg
                    let name = "Photo." + (type.preferredFilenameExtension ?? "jpg")
                    addPhoto(data, name, type.preferredMIMEType ?? "image/jpeg")
                }
            }
        }
    }
}

enum MediaKind {
    case image, video, audio, pdf, text, other

    init(mimeType: String?) {
        let mime = mimeType?.lowercased() ?? ""
        if mime.hasPrefix("image/") { self = .image }
        else if mime.hasPrefix("video/") { self = .video }
        else if mime.hasPrefix("audio/") { self = .audio }
        else if mime == "application/pdf" { self = .pdf }
        else if mime.hasPrefix("text/") || mime.contains("json") { self = .text }
        else { self = .other }
    }

    var symbol: String {
        switch self {
        case .image: "photo"
        case .video: "film"
        case .audio: "waveform"
        case .pdf: "doc.richtext"
        case .text: "doc.text"
        case .other: "doc"
        }
    }
}

enum PlatformImage {
    /// Decodes a downsampled thumbnail so large images never inflate to full size in memory.
    static func make(_ data: Data, maxPixelSize: Int = 720) -> Image? {
        let options = [kCGImageSourceShouldCache: false] as CFDictionary
        guard let source = CGImageSourceCreateWithData(data as CFData, options) else { return nil }
        let thumbnailOptions = [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceCreateThumbnailWithTransform: true,
            kCGImageSourceShouldCacheImmediately: true,
            kCGImageSourceThumbnailMaxPixelSize: maxPixelSize,
        ] as CFDictionary
        guard let image = CGImageSourceCreateThumbnailAtIndex(source, 0, thumbnailOptions) else { return nil }
        return Image(decorative: image, scale: 1)
    }
}

/// Generated media in the transcript; bytes come only from the session's media endpoint via `load`.
struct GeneratedMediaTile: View {
    let identity: String
    let name: String
    let mimeType: String?
    let load: () async throws -> Data

    @State private var data: Data?
    @State private var loadedIdentity: String?
    @State private var failed = false
    @State private var fileURL: URL?
    @State private var quickLookURL: URL?
    @State private var visible = false
    // ShareLink has no completion callback, so a shared copy is left for the cache sweep.
    @State private var shared = false

    private var kind: MediaKind { MediaKind(mimeType: mimeType) }

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.xs) {
            content
                .frame(maxWidth: 360, maxHeight: kind == .image ? 280 : 64, alignment: .leading)
            HStack(spacing: Metrics.s) {
                Text(name).font(.caption).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                Spacer(minLength: 0)
                if let fileURL {
                    Button("Quick Look", systemImage: "eye") { quickLookURL = fileURL }
                        .labelStyle(.iconOnly)
                        .buttonStyle(.borderless)
                    ShareLink(item: fileURL) { Image(systemName: "square.and.arrow.up") }
                        .buttonStyle(.borderless)
                        .accessibilityLabel("Share")
                        .simultaneousGesture(TapGesture().onEnded { shared = true })
                }
            }
            .frame(maxWidth: 360)
        }
        .task(id: identity) {
            if loadedIdentity != identity {
                cleanUp()
                data = nil
                failed = false
                quickLookURL = nil
            }
            if let data {
                if fileURL == nil { fileURL = GeneratedMediaCache.write(data, name: name) }
            } else if !failed {
                await fetch()
            }
        }
        .quickLookPreview($quickLookURL)
        .onAppear {
            visible = true
            GeneratedMediaCache.sweepOnce()
        }
        .onDisappear {
            visible = false
            if quickLookURL == nil { cleanUp() }
        }
        .onChange(of: quickLookURL) { _, url in
            if url == nil && !visible { cleanUp() }
        }
    }

    @ViewBuilder
    private var content: some View {
        if let data, kind == .image, let image = PlatformImage.make(data, maxPixelSize: 1024) {
            image.resizable().scaledToFit()
                .clipShape(.rect(cornerRadius: Metrics.chipRadius, style: .continuous))
                .accessibilityLabel("Generated image \(name)")
        } else if data != nil {
            Label(name, systemImage: kind.symbol)
                .padding(Metrics.m)
                .cardBackground(.fill.tertiary)
        } else if failed {
            HStack {
                Label("Unavailable", systemImage: "exclamationmark.triangle")
                    .foregroundStyle(.secondary)
                Button("Retry") { Task { await fetch() } }.buttonStyle(.borderless)
            }
            .padding(Metrics.m)
            .cardBackground(.fill.quaternary)
        } else {
            ProgressView()
                .frame(width: 120, height: 64)
                .cardBackground(.fill.quaternary)
        }
    }

    private func fetch() async {
        let requested = identity
        failed = false
        do {
            let bytes = try await load()
            guard !Task.isCancelled, requested == identity else { return }
            data = bytes
            loadedIdentity = requested
            fileURL = GeneratedMediaCache.write(bytes, name: name)
        } catch {
            guard !Task.isCancelled, requested == identity else { return }
            failed = true
        }
    }

    private func cleanUp() {
        guard let url = fileURL else { return }
        fileURL = nil
        if shared {
            shared = false
            return
        }
        GeneratedMediaCache.remove(url)
    }
}

/// Temporary copies for Quick Look and Share. Owner-only files in the app's caches;
/// shared copies have no guaranteed lifetime and are swept after a day.
enum GeneratedMediaCache {
    private static let maxAge: TimeInterval = 24 * 60 * 60
    @MainActor private static var swept = false

    private static var root: URL? {
        FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask).first?
            .appending(path: "GeneratedMedia", directoryHint: .isDirectory)
    }

    static func write(_ data: Data, name: String) -> URL? {
        let base = (name as NSString).lastPathComponent
        let safe = String(base.unicodeScalars.filter { !CharacterSet.controlCharacters.contains($0) && $0 != "/" && $0 != "\\" && $0 != ":" })
        let fileName = safe.isEmpty || safe.hasPrefix(".") ? "media" : String(safe.prefix(120))
        guard let root else { return nil }
        let directory = root.appending(path: UUID().uuidString, directoryHint: .isDirectory)
        let manager = FileManager.default
        do {
            try manager.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let url = directory.appending(path: fileName)
            #if os(iOS)
            try data.write(to: url, options: [.atomic, .completeFileProtection])
            #else
            try data.write(to: url, options: [.atomic])
            #endif
            try manager.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
            return url
        } catch {
            try? manager.removeItem(at: directory)
            return nil
        }
    }

    static func remove(_ url: URL) {
        try? FileManager.default.removeItem(at: url.deletingLastPathComponent())
    }

    /// Removes copies left over from earlier shares or launches.
    @MainActor static func sweepOnce() {
        guard !swept, let root else { return }
        swept = true
        Task.detached(priority: .utility) {
            let manager = FileManager.default
            guard let entries = try? manager.contentsOfDirectory(at: root, includingPropertiesForKeys: [.creationDateKey]) else { return }
            let cutoff = Date().addingTimeInterval(-maxAge)
            for entry in entries {
                let created = (try? entry.resourceValues(forKeys: [.creationDateKey]).creationDate) ?? .distantPast
                if created < cutoff { try? manager.removeItem(at: entry) }
            }
        }
    }
}

/// Re-encodes a user-picked photo as JPEG small enough for an inline attachment.
enum ImageDownscaler {
    static func jpeg(from data: Data, maxBytes: Int) -> Data? {
        guard let source = CGImageSourceCreateWithData(data as CFData, [kCGImageSourceShouldCache: false] as CFDictionary) else { return nil }
        for (dimension, quality) in [(2000, 0.8), (1600, 0.75), (1280, 0.7), (1024, 0.65), (800, 0.6)] {
            let options = [
                kCGImageSourceCreateThumbnailFromImageAlways: true,
                kCGImageSourceCreateThumbnailWithTransform: true,
                kCGImageSourceThumbnailMaxPixelSize: dimension,
            ] as CFDictionary
            guard let image = CGImageSourceCreateThumbnailAtIndex(source, 0, options) else { return nil }
            let output = NSMutableData()
            guard let destination = CGImageDestinationCreateWithData(output, UTType.jpeg.identifier as CFString, 1, nil) else { return nil }
            // Thumbnails drop metadata such as location, which is intended for shared photos.
            CGImageDestinationAddImage(destination, image, [kCGImageDestinationLossyCompressionQuality: quality] as CFDictionary)
            guard CGImageDestinationFinalize(destination) else { return nil }
            if output.length <= maxBytes { return output as Data }
        }
        return nil
    }
}
