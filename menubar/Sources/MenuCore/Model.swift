import Foundation

public enum DesktopState: Equatable {
    case on(until: Date?, untilOff: Bool)
    case off
    case unknown
}

public struct Snapshot: Equatable {
    public var desktop: DesktopState = .unknown
    public var paused: Bool? = nil
    public var trusted: Bool? = nil
    public var lastAction: String? = nil
    public var companionReachable = false

    public init() {}
}

let isoFractional: ISO8601DateFormatter = {
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return f
}()

func parseDate(_ value: Any?) -> Date? {
    guard let s = value as? String else { return nil }
    return isoFractional.date(from: s) ?? ISO8601DateFormatter().date(from: s)
}

func object(_ data: Data) -> [String: Any]? {
    (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
}

/// Builds what the menu shows from two CLI results. Anything unreadable is
/// unknown, never "off", and stale facts are not carried over.
public func makeSnapshot(status: CLIResult, desktop: CLIResult, clock: DateFormatter) -> Snapshot {
    var s = Snapshot()
    if status.succeeded, let st = object(status.output) {
        s.companionReachable = true
        s.paused = st["paused"] as? Bool
        if let ax = st["accessibility"] as? [String: Any] { s.trusted = ax["trusted"] as? Bool }
    }
    guard desktop.succeeded, let d = object(desktop.output), let on = d["on"] as? Bool else {
        return s
    }
    if on {
        let untilOff = (d["until_off"] as? Bool) ?? false
        let until = parseDate(d["until"])
        s.desktop = (untilOff || until != nil) ? .on(until: until, untilOff: untilOff) : .unknown
    } else {
        s.desktop = .off
    }
    if let last = d["last_action"] as? [String: Any],
       let app = last["app"] as? String, let command = last["command"] as? String {
        let who = (last["resident"] as? String) ?? "?"
        let when = parseDate(last["time"]).map { clock.string(from: $0) } ?? ""
        s.lastAction = "Last: \(who) · \(command) in \(app) · \(when)"
    }
    return s
}

/// Runs authority changes one at a time in the order the owner asked for them,
/// and tells observations apart by generation so an older status read can
/// never overwrite what was read after a change.
public final class Coordinator {
    private let queue = DispatchQueue(label: "house.souls.menu.authority")
    private let lock = NSLock()
    private var generation = 0
    private var pending = 0

    public init() {}

    /// The generation a refresh starts at; pass it back to `isCurrent`.
    public func beginObservation() -> Int {
        lock.lock(); defer { lock.unlock() }
        return generation
    }

    public func isCurrent(_ observed: Int) -> Bool {
        lock.lock(); defer { lock.unlock() }
        return observed == generation && pending == 0
    }

    public var busy: Bool {
        lock.lock(); defer { lock.unlock() }
        return pending > 0
    }

    public func change(_ work: @escaping () -> Bool, done: @escaping (Bool) -> Void) {
        lock.lock(); pending += 1; generation += 1; lock.unlock()
        queue.async {
            let ok = work()
            self.lock.lock(); self.pending -= 1; self.generation += 1; self.lock.unlock()
            done(ok)
        }
    }
}
