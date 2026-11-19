import Foundation

/// Where the app keeps the companion it carries, and where it installs it.
public struct CompanionInstall {
    /// The companion inside this app (Contents/MacOS/souls-house-companion).
    public let bundled: String
    /// The installed companion the LaunchAgent runs (~/.local/bin/souls-house-companion).
    public let installed: String
    /// The companion's LaunchAgent plist (written by `install --user`).
    public let launchAgent: String
    /// The companion's enrollment state. Only its presence is ever checked.
    public let state: String

    public init(bundled: String, installed: String, launchAgent: String, state: String) {
        self.bundled = bundled
        self.installed = installed
        self.launchAgent = launchAgent
        self.state = state
    }

    public init(bundled: String, home: String) {
        self.init(bundled: bundled,
                  installed: (home as NSString).appendingPathComponent(".local/bin/souls-house-companion"),
                  launchAgent: (home as NSString).appendingPathComponent("Library/LaunchAgents/house.souls.companion.plist"),
                  state: (home as NSString).appendingPathComponent(".local/share/souls-house-companion/state.json"))
    }

    /// True when the binary is gone but an earlier setup's LaunchAgent or
    /// enrollment is still there (or can't be checked). Installing then would
    /// put a binary back under an old login job and an old enrollment.
    public var leftovers: Bool {
        present(launchAgent) != false || present(state) != false
    }

    /// True when nothing is installed yet, so the owner is asked before the
    /// first install. A symlink counts as present (and is then refused).
    public var needsFirstInstall: Bool {
        (try? FileManager.default.attributesOfItem(atPath: installed)) == nil
    }
}

public enum InstallOutcome: Equatable {
    case upToDate
    case installed(version: String)
    /// A different companion is already installed. This release only installs
    /// fresh: it leaves an existing installation exactly as it is.
    case existingInstall(version: String?)
    /// No companion, but an earlier setup's login job or enrollment remains.
    case leftovers
    case failed(String)

    /// One line for the menu, or nil when there is nothing to say.
    public var note: String? {
        switch self {
        case .upToDate: return nil
        case let .installed(v): return "Companion installed (\(v))"
        case .existingInstall: return "An earlier companion is installed; left as it is"
        case .leftovers: return "An earlier setup's files remain; nothing installed"
        case let .failed(why): return why
        }
    }
}

func versionOf(_ path: String, _ run: (String, [String]) -> CLIResult) -> String? {
    let r = run(path, ["version"])
    guard r.succeeded, let s = String(data: r.output, encoding: .utf8) else { return nil }
    let v = s.trimmingCharacters(in: .whitespacesAndNewlines)
    return v.isEmpty ? nil : v
}

/// Whether something is at `path` (not following symlinks): nil when that
/// can't be told, which callers treat as present.
func present(_ path: String) -> Bool? {
    var st = stat()
    if lstat(path, &st) == 0 { return true }
    return errno == ENOENT ? false : nil
}

func isSymlink(_ path: String) -> Bool {
    ((try? FileManager.default.attributesOfItem(atPath: path))?[.type] as? FileAttributeType) == .typeSymbolicLink
}

/// Fresh installs only (updates come in a later release, with their own
/// review). When nothing is installed: copy the companion this app carries
/// next to its destination, publish it with a hard link (which refuses a
/// target that appeared meanwhile, so nothing is ever overwritten), then
/// `install --user`: configuration only, not enrolled or started.
///
/// When the same version is installed, only a missing LaunchAgent is written.
/// When anything else is there (another version, or a companion that doesn't
/// answer), nothing is touched and `.existingInstall` says so.
public func ensureCompanion(_ paths: CompanionInstall,
                            run: (String, [String]) -> CLIResult) -> InstallOutcome {
    let fm = FileManager.default
    guard let bundledVersion = versionOf(paths.bundled, run) else {
        return .failed("The companion inside this app didn't start")
    }
    let target = paths.installed
    let dir = (target as NSString).deletingLastPathComponent
    if isSymlink(target) || isSymlink(dir) || isSymlink((dir as NSString).deletingLastPathComponent) {
        return .failed("Not installing: \(dir) is a symlink")
    }
    if !paths.needsFirstInstall {
        let installed = versionOf(target, run)
        guard installed == bundledVersion else { return .existingInstall(version: installed) }
        return configure(paths, run) ?? .upToDate
    }
    if paths.leftovers { return .leftovers }
    let staging = target + ".staging"
    do {
        try fm.createDirectory(atPath: dir, withIntermediateDirectories: true)
        if (try? fm.attributesOfItem(atPath: staging)) != nil { try fm.removeItem(atPath: staging) }
        try fm.copyItem(atPath: paths.bundled, toPath: staging)
        try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: staging)
        defer { try? fm.removeItem(atPath: staging) }
        try fm.linkItem(atPath: staging, toPath: target)
    } catch {
        try? fm.removeItem(atPath: staging)
        return .failed("Couldn't put the companion in \(dir)")
    }
    guard versionOf(target, run) == bundledVersion else {
        return .failed("The installed companion didn't answer after copying")
    }
    return configure(paths, run) ?? .installed(version: bundledVersion)
}

/// Writes the companion's LaunchAgent when it is missing. Returns a failure,
/// or nil when a complete configuration is in place. A plist that exists but
/// isn't the companion's whole job (a write that failed half way) is a
/// failure, never success, and a failed `install` is a failure whatever it
/// left behind.
func configure(_ paths: CompanionInstall, _ run: (String, [String]) -> CLIResult) -> InstallOutcome? {
    let broken = InstallOutcome.failed("The companion is installed but its login item couldn't be written")
    switch present(paths.launchAgent) {
    case true?: return launchAgentValid(paths) ? nil : broken
    case nil: return broken
    case false?: break
    }
    guard run(paths.installed, ["install", "--user"]).succeeded, launchAgentValid(paths) else { return broken }
    return nil
}

/// The plist parses and runs this companion under the companion's label.
func launchAgentValid(_ paths: CompanionInstall) -> Bool {
    guard !isSymlink(paths.launchAgent),
          let data = FileManager.default.contents(atPath: paths.launchAgent),
          let plist = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any],
          plist["Label"] as? String == "house.souls.companion",
          let args = plist["ProgramArguments"] as? [String],
          args.first == paths.installed, args.dropFirst().first == "serve" else { return false }
    return true
}

/// True when this Mac was set up by an earlier release (or by hand): a
/// companion this app didn't put there, or the old menu's LaunchAgent. This
/// release then changes nothing and says so.
public func earlierSetup(_ paths: CompanionInstall, legacyMenuAgent: String,
                         run: (String, [String]) -> CLIResult) -> Bool {
    if (try? FileManager.default.attributesOfItem(atPath: legacyMenuAgent)) != nil { return true }
    if paths.needsFirstInstall { return paths.leftovers }
    guard let bundled = versionOf(paths.bundled, run) else { return true }
    return versionOf(paths.installed, run) != bundled
}
