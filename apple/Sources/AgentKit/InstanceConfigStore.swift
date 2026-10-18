import Foundation

public final class InstanceConfigStore: @unchecked Sendable {
    private let fileURL: URL
    private let lock = NSLock()
    private let encoder = JSONEncoder()
    private let decoder = JSONDecoder()
    public init(fileManager: FileManager = .default, fileURL: URL? = nil) {
        if let fileURL { self.fileURL = fileURL }
        else { let root = fileManager.urls(for: .applicationSupportDirectory, in: .userDomainMask).first ?? fileManager.temporaryDirectory; self.fileURL = root.appending(path: "DockerAgentApple/instances.json") }
    }
    public func load() -> [InstanceConfig] { lock.lock(); defer { lock.unlock() }; guard let data = try? Data(contentsOf: fileURL), let configs = try? decoder.decode([InstanceConfig].self, from: data) else { return [] }; return configs }
    public func upsert(_ config: InstanceConfig) throws { lock.lock(); defer { lock.unlock() }; var configs = readUnlocked(); configs.removeAll { $0.id == config.id }; configs.append(config); try writeUnlocked(configs) }
    public func remove(_ id: UUID) throws { lock.lock(); defer { lock.unlock() }; try writeUnlocked(readUnlocked().filter { $0.id != id }) }
    private func readUnlocked() -> [InstanceConfig] { guard let data = try? Data(contentsOf: fileURL) else { return [] }; return (try? decoder.decode([InstanceConfig].self, from: data)) ?? [] }
    private func writeUnlocked(_ configs: [InstanceConfig]) throws { let data = try encoder.encode(configs); try FileManager.default.createDirectory(at: fileURL.deletingLastPathComponent(), withIntermediateDirectories: true); try data.write(to: fileURL, options: [.atomic]) }
}
