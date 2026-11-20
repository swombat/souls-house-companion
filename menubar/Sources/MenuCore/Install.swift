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
    /// An older companion was replaced by the one this app carries. Its
    /// enrollment and LaunchAgent are kept; the caller restarts the service.
    case updated(from: String, to: String)
    /// A companion this app won't replace: a newer one, or one that doesn't
    /// answer. It is left exactly as it is.
    case existingInstall(version: String?)
    /// No companion, but an earlier setup's login job or enrollment remains.
    case leftovers
    case failed(String)

    /// One line for the menu, or nil when there is nothing to say.
    public var note: String? {
        switch self {
        case .upToDate: return nil
        case let .installed(v): return "Companion installed (\(v))"
        case let .updated(_, to): return "Companion updated to \(to)"
        case .existingInstall: return "A different companion is installed; left as it is"
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

/// Companions shipped inside the Mac app before releases were stamped with
/// their tag: they print the 12-character revision of the tagged commit.
let shippedRevisions = [
    "da40108395bb": "v0.3.0",
    "ad70d2ecb977": "v0.4.0",
    "92d1e67d0545": "v0.4.1",
]

/// Release versions ("v0.4.1", or a known shipped revision) as numbers; nil
/// for anything else ("development", "dev-abc1234", another revision), which
/// is never compared and so never replaced.
func releaseNumbers(_ raw: String) -> [Int]? {
    let v = shippedRevisions[raw] ?? raw
    guard v.hasPrefix("v") else { return nil }
    let parts = v.dropFirst().split(separator: ".", omittingEmptySubsequences: false).map { Int($0) }
    guard parts.count == 3, parts.allSatisfy({ $0 != nil }) else { return nil }
    return parts.map { $0! }
}

/// True only when both are release versions and `bundled` is newer.
public func isUpgrade(from installed: String, to bundled: String) -> Bool {
    guard let a = releaseNumbers(installed), let b = releaseNumbers(bundled) else { return false }
    return a.lexicographicallyPrecedes(b)
}

/// When nothing is installed: copy the companion this app carries next to
/// its destination, publish it with a hard link (which refuses a target that
/// appeared meanwhile), then `install --user`: configuration only, not
/// enrolled or started.
///
/// When the same version is installed, only a missing LaunchAgent is written.
/// When an older release is installed, it is replaced by renaming the new
/// copy over it (the old binary is kept as `.previous`); enrollment and
/// LaunchAgent stay, and `.updated` tells the caller to restart the service.
/// When anything else is there (a newer or unversioned companion, or one that
/// doesn't answer), nothing is touched and `.existingInstall` says so.
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
        if installed == bundledVersion { return configure(paths, run) ?? .upToDate }
        guard let old = installed, isUpgrade(from: old, to: bundledVersion) else {
            return .existingInstall(version: installed)
        }
        return update(paths, from: old, to: bundledVersion, run)
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

/// Replaces an older installed companion with the bundled one. The new copy
/// is staged beside it and renamed over it, so the path always holds a whole
/// binary; the old one is kept as `.previous` and put back if the new one
/// doesn't answer.
func update(_ paths: CompanionInstall, from old: String, to new: String,
            _ run: (String, [String]) -> CLIResult) -> InstallOutcome {
    let fm = FileManager.default
    let target = paths.installed
    let staging = target + ".staging"
    let previous = target + ".previous"
    do {
        if (try? fm.attributesOfItem(atPath: staging)) != nil { try fm.removeItem(atPath: staging) }
        try fm.copyItem(atPath: paths.bundled, toPath: staging)
        try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: staging)
        if (try? fm.attributesOfItem(atPath: previous)) != nil { try fm.removeItem(atPath: previous) }
        try fm.linkItem(atPath: target, toPath: previous)
        guard rename(staging, target) == 0 else { throw CocoaError(.fileWriteUnknown) }
    } catch {
        try? fm.removeItem(atPath: staging)
        return .failed("Couldn't update the companion")
    }
    guard versionOf(target, run) == new else {
        // Put the old binary back by linking, so `.previous` stays as a
        // recovery copy whatever happens here.
        let restored = unlink(target) == 0 && link(previous, target) == 0
            && versionOf(target, run) == old
        return .failed(restored
            ? "The updated companion didn't answer; the earlier one was put back"
            : "The updated companion didn't answer and the earlier one couldn't be put back; it is kept at \(previous)")
    }
    return configure(paths, run) ?? .updated(from: old, to: new)
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

/// True when this Mac was set up in a way this app won't touch: the old
/// menu's LaunchAgent, leftovers without a binary, or a companion that
/// doesn't answer. An older release is not one of these: it gets updated.
/// A newer one isn't either: it is left alone and the menu says so.
public func earlierSetup(_ paths: CompanionInstall, legacyMenuAgent: String,
                         run: (String, [String]) -> CLIResult) -> Bool {
    if (try? FileManager.default.attributesOfItem(atPath: legacyMenuAgent)) != nil { return true }
    if paths.needsFirstInstall { return paths.leftovers }
    guard versionOf(paths.bundled, run) != nil else { return true }
    return versionOf(paths.installed, run) == nil
}

/// The pid in `launchctl print` output when the job is running, else nil.
func runningPID(_ printed: Data) -> String? {
    guard let text = String(data: printed, encoding: .utf8),
          text.contains("state = running") else { return nil }
    for line in text.split(separator: "\n") {
        let t = line.trimmingCharacters(in: .whitespaces)
        if t.hasPrefix("pid = ") { return String(t.dropFirst(6)) }
    }
    return nil
}

/// After an update, restarts the companion's launchd job so the new binary
/// runs. Returns nil when there was nothing to restart (launchd reports the
/// job absent: never loaded) or when a new process is seen running; otherwise
/// a line for the menu. Exit 113 is launchctl's "could not find service";
/// any other failure to look is reported, not taken for absence.
public func restartAfterUpdate(job: String, run: (String, [String]) -> CLIResult) -> String? {
    let launchctl = "/bin/launchctl"
    let before = run(launchctl, ["print", job])
    if before.outcome == .exited(113) { return nil }
    guard before.succeeded else { return "Updated, but couldn't check whether the companion is running" }
    let oldPID = runningPID(before.output)
    guard run(launchctl, ["kickstart", "-k", job]).succeeded else {
        return "Updated, but couldn't restart the companion; log out and in to finish"
    }
    for _ in 0..<10 {
        let after = run(launchctl, ["print", job])
        if after.succeeded, let pid = runningPID(after.output), pid != oldPID { return nil }
        Thread.sleep(forTimeInterval: 0.5)
    }
    return "Updated, but the companion didn't come back running; log out and in to finish"
}
