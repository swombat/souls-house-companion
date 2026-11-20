import Foundation

/// What happened when the owner turned "Let residents run commands on this
/// Mac" on or off. Every case but `.done` is shown in the menu.
public enum LifecycleChange: Equatable {
    /// Changed, confirmed by a fresh status read, and the companion is back
    /// in the pause state it was in before.
    case done
    /// The companion's state could not be read, so nothing was tried.
    case couldNotRead
    /// The companion could not be paused, so nothing was changed.
    case couldNotPause
    /// A readable status shows the change did not happen. The companion was
    /// put back as it was.
    case notChanged
    /// A readable status shows the change did not happen, and the companion
    /// also failed to resume.
    case notChangedAndPaused
    /// The change was confirmed but the companion failed to resume.
    case changedButPaused
    /// Turning commands off could not be confirmed. The companion stays
    /// paused rather than resuming into the old permission.
    case revokeUnconfirmed(resumeOwed: Bool)
    /// Allowing commands was tried but status could not be read afterwards,
    /// so nobody knows whether commands are on. The companion stays paused.
    case acceptUnverified(resumeOwed: Bool)

    /// The state the menu must hold after this outcome, if any.
    public var hold: LifecycleHold? {
        switch self {
        case let .revokeUnconfirmed(owed): return .revokeUnconfirmed(resumeOwed: owed)
        case let .acceptUnverified(owed): return .acceptUnverified(resumeOwed: owed)
        default: return nil
        }
    }
}

/// An unresolved change. The companion is paused, the permission is either
/// still on or unknown, and the menu offers no ordinary Resume until it is
/// resolved. `resumeOwed` is true when the menu itself paused the companion,
/// so resolving it should resume.
public enum LifecycleHold: Equatable {
    case revokeUnconfirmed(resumeOwed: Bool)
    case acceptUnverified(resumeOwed: Bool)

    var resumeOwed: Bool {
        switch self {
        case let .revokeUnconfirmed(owed), let .acceptUnverified(owed): return owed
        }
    }

    /// The outcome that leaves this hold exactly as it is.
    var unchanged: LifecycleChange {
        switch self {
        case let .revokeUnconfirmed(owed): return .revokeUnconfirmed(resumeOwed: owed)
        case let .acceptUnverified(owed): return .acceptUnverified(resumeOwed: owed)
        }
    }

    /// A string form for keeping the hold across a restart of the menu.
    public var stored: String {
        switch self {
        case let .revokeUnconfirmed(owed): return "revoke-unconfirmed:\(owed ? 1 : 0)"
        case let .acceptUnverified(owed): return "accept-unverified:\(owed ? 1 : 0)"
        }
    }

    public init?(stored: String) {
        switch stored {
        case "revoke-unconfirmed:1": self = .revokeUnconfirmed(resumeOwed: true)
        case "revoke-unconfirmed:0": self = .revokeUnconfirmed(resumeOwed: false)
        case "accept-unverified:1": self = .acceptUnverified(resumeOwed: true)
        case "accept-unverified:0": self = .acceptUnverified(resumeOwed: false)
        default: return nil
        }
    }

    /// The line the menu shows while the hold lasts.
    public var message: String {
        switch self {
        case .revokeUnconfirmed:
            return "Could not confirm commands are off; the companion stays paused until they are"
        case .acceptUnverified:
            return "Could not check whether commands were allowed; the companion stays paused until it can"
        }
    }
}

/// Turns the Mac's local run-commands opt-in on or off without Terminal.
///
/// Accepting or revoking needs the companion's state lock, which the running
/// service holds, so this pauses first, changes the acceptance, reads status
/// back, and resumes. A pause that was already in place (or that cannot be
/// read) stays in place unless `resumeOwed` says this menu made it. Nothing
/// is resumed unless a readable status says what the permission now is, and
/// a revoke is only resumed from once status shows it is gone. The contract
/// accepted is the one the companion itself reports, and the companion still
/// checks it exactly.
///
/// `run` is the companion CLI; it is a parameter so tests can stand in for it.
public func setExecuteLifecycle(allow: Bool, resumeOwed: Bool = false,
                                run: ([String]) -> CLIResult) -> LifecycleChange {
    let before = run(["status", "--json"])
    guard before.succeeded, let st = object(before.output),
          let contract = st["execute_lifecycle"] as? String, !contract.isEmpty else {
        return .couldNotRead
    }
    let wasRunning = (st["paused"] as? Bool) == false
    if wasRunning, !run(["pause"]).succeeded {
        return .couldNotPause
    }
    // Only a pause this menu made is undone afterwards.
    let resumeAfter = wasRunning || resumeOwed
    // --paired-residents: the owner's confirmation in this menu is what sets
    // (or clears) commands for the residents paired to this Mac.
    _ = run(allow ? ["execute-lifecycle", "--accept-contract", contract, "--paired-residents"]
                  : ["execute-lifecycle", "--revoke", "--paired-residents"])
    let accepted = readAccepted(run)
    let confirmed: Bool
    if allow {
        guard let accepted else { return .acceptUnverified(resumeOwed: resumeAfter) }
        confirmed = accepted == contract
    } else {
        guard accepted == "" else { return .revokeUnconfirmed(resumeOwed: resumeAfter) }
        confirmed = true
    }
    return finish(confirmed: confirmed, resume: resumeAfter, run: run)
}

/// Reads status again after `.acceptUnverified`. Still unreadable keeps the
/// hold; readable resolves it and resumes if the menu owes a resume.
public func checkAcceptAgain(resumeOwed: Bool, run: ([String]) -> CLIResult) -> LifecycleChange {
    let st = run(["status", "--json"])
    guard st.succeeded, let o = object(st.output),
          let contract = o["execute_lifecycle"] as? String, !contract.isEmpty,
          let accepted = o["accepted_execute_lifecycle"] as? String else {
        return .acceptUnverified(resumeOwed: resumeOwed)
    }
    return finish(confirmed: accepted == contract, resume: resumeOwed, run: run)
}

/// Resolves a hold the way the owner chose from the menu. A hold ends only on
/// an outcome that read the permission back: a confirmed revoke, or a
/// readable check. Anything inconclusive (status unreadable before the retry,
/// a pause that fails) keeps the hold it started from, so a later healthy
/// refresh can never turn it into a plain Resume over the old permission.
public func resolveHold(_ hold: LifecycleHold, turnOff: Bool, run: ([String]) -> CLIResult) -> LifecycleChange {
    let outcome = turnOff ? setExecuteLifecycle(allow: false, resumeOwed: hold.resumeOwed, run: run)
                          : checkAcceptAgain(resumeOwed: hold.resumeOwed, run: run)
    switch outcome {
    case .couldNotRead, .couldNotPause: return hold.unchanged
    default: return outcome
    }
}

private func readAccepted(_ run: ([String]) -> CLIResult) -> String? {
    let r = run(["status", "--json"])
    guard r.succeeded, let o = object(r.output) else { return nil }
    return o["accepted_execute_lifecycle"] as? String
}

private func finish(confirmed: Bool, resume: Bool, run: ([String]) -> CLIResult) -> LifecycleChange {
    guard resume else { return confirmed ? .done : .notChanged }
    if !run(["resume"]).succeeded {
        return confirmed ? .changedButPaused : .notChangedAndPaused
    }
    return confirmed ? .done : .notChanged
}

/// The line the menu shows for an outcome, or nil when there is nothing to
/// say. Held outcomes are described by their `LifecycleHold.message`.
public func lifecycleMessage(_ change: LifecycleChange, allow: Bool) -> String? {
    switch change {
    case .done, .revokeUnconfirmed, .acceptUnverified: return nil
    case .couldNotRead: return "Could not read the companion's state; nothing changed"
    case .couldNotPause: return "Could not pause the companion; nothing changed"
    case .notChanged: return allow ? "Could not allow commands; nothing changed" : "Could not turn commands off"
    case .notChangedAndPaused:
        return allow ? "Could not allow commands, and the companion is paused"
                     : "Could not turn commands off, and the companion is paused"
    case .changedButPaused:
        return allow ? "Commands allowed, but the companion is paused" : "Commands off, but the companion is paused"
    }
}

/// One entry in the run-commands part of the menu.
public enum LifecycleMenuItem: Equatable {
    /// The "Let residents run commands on this Mac" switch.
    case runCommands(checked: Bool, enabled: Bool)
    case note(String)
    /// Retry turning commands off after a revoke that was not confirmed.
    case tryTurnOffAgain
    /// Read status again after an accept that could not be checked.
    case checkAgain
    /// Turn commands off instead of checking again.
    case turnOffInstead
    /// The ordinary Pause / Resume companion item.
    case pauseOrResume(resume: Bool)
}

public extension LifecycleMenuItem {
    var title: String {
        switch self {
        case .runCommands: return "Let residents run commands on this Mac"
        case let .note(text): return text
        case .tryTurnOffAgain: return "Try again to turn commands off"
        case .checkAgain: return "Check again"
        case .turnOffInstead: return "Turn commands off instead"
        case let .pauseOrResume(resume): return resume ? "Resume companion" : "Pause companion"
        }
    }
}

/// What the run-commands and pause part of the menu offers.
///
/// - `busy`: a change is running; nothing is clickable.
/// - `awaitingRead`: a change finished but no status read after it has
///   landed yet, so the old snapshot must not drive the switch or the
///   pause item.
/// - `hold`: an unresolved change; only the actions that resolve it are
///   offered, never an ordinary Resume.
public func lifecycleMenuItems(executeAccepted: Bool?, paused: Bool?, busy: Bool, awaitingRead: Bool,
                               hold: LifecycleHold?, note: String?) -> [LifecycleMenuItem] {
    if let hold {
        var items: [LifecycleMenuItem] = [.note(hold.message)]
        guard !busy else { return items }
        switch hold {
        case .revokeUnconfirmed: items.append(.tryTurnOffAgain)
        case .acceptUnverified: items += [.checkAgain, .turnOffInstead]
        }
        return items
    }
    var items: [LifecycleMenuItem] = []
    if let executeAccepted {
        items.append(.runCommands(checked: executeAccepted, enabled: !busy && !awaitingRead))
    }
    if let note { items.append(.note(note)) }
    if !busy, !awaitingRead, let paused { items.append(.pauseOrResume(resume: paused)) }
    return items
}
