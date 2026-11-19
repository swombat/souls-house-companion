import Foundation

/// Where the app keeps the companion it carries, and where it installs it.
public struct CompanionInstall {
    /// The companion inside this app (Contents/MacOS/souls-house-companion).
    public let bundled: String
    /// The installed companion the LaunchAgent runs (~/.local/bin/souls-house-companion).
    public let installed: String
    /// The companion's LaunchAgent plist (written by `install --user`).
    public let launchAgent: String

    public init(bundled: String, installed: String, launchAgent: String) {
        self.bundled = bundled
        self.installed = installed
        self.launchAgent = launchAgent
    }

    public init(bundled: String, home: String) {
        self.init(bundled: bundled,
                  installed: (home as NSString).appendingPathComponent(".local/bin/souls-house-companion"),
                  launchAgent: (home as NSString).appendingPathComponent("Library/LaunchAgents/house.souls.companion.plist"))
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
    case updated(from: String, to: String)
    case failed(String)

    /// One line for the menu, or nil when there is nothing to say.
    public var note: String? {
        switch self {
        case .upToDate: return nil
        case let .installed(v): return "Companion installed (\(v))"
        case let .updated(from, to): return "Companion updated (\(from) → \(to))"
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

func isSymlink(_ path: String) -> Bool {
    ((try? FileManager.default.attributesOfItem(atPath: path))?[.type] as? FileAttributeType) == .typeSymbolicLink
}

/// Copies `source` next to `target` and makes it executable, so the final
/// step is a rename on one volume.
func stage(_ source: String, beside target: String) throws -> String {
    let staging = target + ".staging"
    let fm = FileManager.default
    if (try? fm.attributesOfItem(atPath: staging)) != nil { try fm.removeItem(atPath: staging) }
    try fm.copyItem(atPath: source, toPath: staging)
    try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: staging)
    return staging
}

/// Puts the companion this app carries in place, or brings an installed one
/// up to it. Never needs sudo and never runs as root.
///
/// First install: copy, then `install --user` (configuration only: it is not
/// enrolled or started). Update: when the installed version differs, a
/// running companion is paused, the old binary kept as `.previous`, the new
/// one moved in and the companion resumed; if it won't resume, the previous
/// binary goes back and is resumed instead. A companion the owner had paused
/// stays paused. Any failure before the swap changes nothing.
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

    if paths.needsFirstInstall {
        do {
            try fm.createDirectory(atPath: dir, withIntermediateDirectories: true)
            let staging = try stage(paths.bundled, beside: target)
            defer { try? fm.removeItem(atPath: staging) }
            // A hard link refuses a target that appeared meanwhile: never overwrite.
            try fm.linkItem(atPath: staging, toPath: target)
        } catch {
            return .failed("Couldn't put the companion in \(dir)")
        }
        return configure(paths, run) ?? .installed(version: bundledVersion)
    }

    guard let installedVersion = versionOf(target, run) else {
        return .failed("The installed companion didn't answer; nothing changed")
    }
    if installedVersion == bundledVersion {
        return configure(paths, run) ?? .upToDate
    }

    // Running means enrolled and not paused. Unreadable status (for example
    // not enrolled yet) means there is no service to pause.
    var running = false
    let status = run(target, ["status", "--json"])
    if status.succeeded, let st = object(status.output) {
        running = (st["enrolled"] as? Bool) == true && (st["paused"] as? Bool) == false
    }
    if running && !run(target, ["pause"]).succeeded {
        return .failed("Couldn't pause the companion to update it; nothing changed")
    }
    let previous = target + ".previous"
    do {
        let staging = try stage(paths.bundled, beside: target)
        if (try? fm.attributesOfItem(atPath: previous)) != nil { try fm.removeItem(atPath: previous) }
        try fm.moveItem(atPath: target, toPath: previous)
        do {
            try fm.moveItem(atPath: staging, toPath: target)
        } catch {
            try? fm.moveItem(atPath: previous, toPath: target)
            try? fm.removeItem(atPath: staging)
            throw error
        }
    } catch {
        if running { _ = run(target, ["resume"]) }
        return .failed("Couldn't update the companion; the previous one is still in place")
    }
    if running && !run(target, ["resume"]).succeeded {
        // Put the old binary back exactly and start it again.
        let failed = target + ".failed"
        try? fm.removeItem(atPath: failed)
        try? fm.moveItem(atPath: target, toPath: failed)
        try? fm.moveItem(atPath: previous, toPath: target)
        let back = run(target, ["resume"]).succeeded
        return .failed(back ? "The updated companion didn't start; the previous one is back and running"
                            : "The updated companion didn't start; the previous one is back but paused")
    }
    return configure(paths, run) ?? .updated(from: installedVersion, to: bundledVersion)
}

/// Writes the companion's LaunchAgent when it is missing. Returns a failure,
/// or nil when the configuration is in place.
func configure(_ paths: CompanionInstall, _ run: (String, [String]) -> CLIResult) -> InstallOutcome? {
    if (try? FileManager.default.attributesOfItem(atPath: paths.launchAgent)) != nil { return nil }
    if run(paths.installed, ["install", "--user"]).succeeded { return nil }
    if (try? FileManager.default.attributesOfItem(atPath: paths.launchAgent)) != nil { return nil }
    return .failed("The companion is installed but its login item couldn't be written")
}
