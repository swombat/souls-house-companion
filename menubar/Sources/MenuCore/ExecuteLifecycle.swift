import Foundation

/// What happened when the owner turned "Let residents run commands on this
/// Mac" on or off. Every case but `.done` is shown in the menu as it is.
public enum LifecycleChange: Equatable {
    /// Changed, confirmed by a fresh status read, and the companion is back
    /// in the pause state it was in before.
    case done
    /// The companion's state could not be read, so nothing was tried.
    case couldNotRead
    /// The companion could not be paused, so nothing was changed.
    case couldNotPause
    /// The change was not confirmed. The companion was put back as it was.
    case notChanged
    /// The change was not confirmed and the companion also failed to resume.
    case notChangedAndPaused
    /// The change was confirmed but the companion failed to resume.
    case changedButPaused
    /// Turning commands off could not be confirmed, so the companion was
    /// left paused rather than resumed into the old permission.
    case revokeUnconfirmedPaused
}

/// Turns the Mac's local run-commands opt-in on or off without Terminal.
///
/// Accepting or revoking needs the companion's state lock, which the running
/// service holds, so this pauses first, changes the acceptance, reads status
/// back, and resumes. A pause that was already in place (or that cannot be
/// read) stays in place. The contract accepted is the one the companion
/// itself reports, and the companion still checks it exactly.
///
/// `run` is the companion CLI; it is a parameter so tests can stand in for it.
public func setExecuteLifecycle(allow: Bool, run: ([String]) -> CLIResult) -> LifecycleChange {
    let before = run(["status", "--json"])
    guard before.succeeded, let st = object(before.output),
          let contract = st["execute_lifecycle"] as? String, !contract.isEmpty else {
        return .couldNotRead
    }
    // Only an explicit "not paused" is resumed afterwards.
    let wasRunning = (st["paused"] as? Bool) == false
    if wasRunning, !run(["pause"]).succeeded {
        return .couldNotPause
    }
    _ = run(allow ? ["execute-lifecycle", "--accept-contract", contract] : ["execute-lifecycle", "--revoke"])
    let after = run(["status", "--json"])
    let accepted = (after.succeeded ? object(after.output) : nil)?["accepted_execute_lifecycle"] as? String
    let confirmed = allow ? accepted == contract : accepted == ""
    if !allow, !confirmed {
        return .revokeUnconfirmedPaused
    }
    guard wasRunning else {
        return confirmed ? .done : .notChanged
    }
    if !run(["resume"]).succeeded {
        return confirmed ? .changedButPaused : .notChangedAndPaused
    }
    return confirmed ? .done : .notChanged
}

/// The line the menu shows for an outcome, or nil when there is nothing to say.
public func lifecycleMessage(_ change: LifecycleChange, allow: Bool) -> String? {
    switch change {
    case .done: return nil
    case .couldNotRead: return "Could not read the companion's state; nothing changed"
    case .couldNotPause: return "Could not pause the companion; nothing changed"
    case .notChanged: return allow ? "Could not allow commands; nothing changed" : "Could not turn commands off"
    case .notChangedAndPaused: return "Could not allow commands, and the companion is paused"
    case .changedButPaused:
        return allow ? "Commands allowed, but the companion is paused" : "Commands off, but the companion is paused"
    case .revokeUnconfirmedPaused: return "Could not confirm commands are off; the companion stays paused"
    }
}
