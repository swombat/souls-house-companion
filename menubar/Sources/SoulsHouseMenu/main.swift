// The souls.house menu bar: the owner's switch for desktop control.
//
// It holds no credentials and talks to nothing but the installed companion
// CLI. Every authority decision (including expiry) is enforced by the
// companion; if this app quits or crashes, control still turns itself off.
import AppKit
import ServiceManagement
import MenuCore

let companion = (NSHomeDirectory() as NSString).appendingPathComponent(".local/bin/souls-house-companion")
// The Computers page on souls.house names this item word for word.
let holdKey = "runCommandsHold"
let accessibilityPane = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility")!

let clock: DateFormatter = {
    let f = DateFormatter()
    f.dateFormat = "HH:mm"
    return f
}()

// The souls.house mark (roof over a coral orb), drawn for the menu bar.
// On: the orb is filled. Off: the orb is a ring. Unknown: the ring is grey.
// The roof uses the label colour so it follows light and dark menu bars.
enum HouseMark {
    case on, off, unknown

    var toolTip: String {
        switch self {
        case .on: return "souls.house: desktop control on"
        case .off: return "souls.house: desktop control off"
        case .unknown: return "souls.house: desktop control state unknown"
        }
    }
}

let houseCoral = NSColor(srgbRed: 0xf1 / 255.0, green: 0x5d / 255.0, blue: 0x61 / 255.0, alpha: 1)

func houseMark(_ mark: HouseMark) -> NSImage {
    // Logo viewBox is 240 x 210; scale it to the menu bar's 18 pt glyph height.
    let scale: CGFloat = 18.0 / 210.0
    let size = NSSize(width: 240 * scale, height: 18)
    let image = NSImage(size: size, flipped: true) { _ in
        let roof = NSBezierPath()
        roof.move(to: NSPoint(x: 16 * scale, y: 100 * scale))
        roof.line(to: NSPoint(x: 120 * scale, y: 24 * scale))
        roof.line(to: NSPoint(x: 224 * scale, y: 100 * scale))
        roof.lineWidth = 26 * scale
        roof.lineCapStyle = .round
        roof.lineJoinStyle = .round
        NSColor.labelColor.setStroke()
        roof.stroke()

        let ringWidth = 22 * scale
        switch mark {
        case .on:
            let r = 58 * scale
            let orb = NSBezierPath(ovalIn: NSRect(x: 120 * scale - r, y: 148 * scale - r, width: 2 * r, height: 2 * r))
            houseCoral.setFill()
            orb.fill()
        case .off, .unknown:
            // Keep the ring inside the filled orb's outline so the mark doesn't grow.
            let r = 58 * scale - ringWidth / 2
            let orb = NSBezierPath(ovalIn: NSRect(x: 120 * scale - r, y: 148 * scale - r, width: 2 * r, height: 2 * r))
            orb.lineWidth = ringWidth
            (mark == .off ? houseCoral : NSColor.secondaryLabelColor).setStroke()
            orb.stroke()
        }
        return true
    }
    image.isTemplate = false
    image.accessibilityDescription = mark.toolTip
    return image
}

final class Menu: NSObject, NSMenuDelegate {
    let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    let menu = NSMenu()
    let coordinator = Coordinator()
    let refreshQueue = DispatchQueue(label: "house.souls.menu.refresh")
    var refreshing = false
    var timer: Timer?
    var snapshot = Snapshot()
    var failedAction: String?
    var lifecycleNote: String?
    /// What setting up or updating the companion did, shown until the next change.
    var installNote: String?
    var inProgress: String?
    /// An unresolved run-commands change, kept across restarts of the menu.
    var hold: LifecycleHold? = UserDefaults.standard.string(forKey: holdKey).flatMap(LifecycleHold.init(stored:)) {
        didSet { UserDefaults.standard.set(hold?.stored, forKey: holdKey) }
    }
    /// True from the end of a run-commands change until a status read made
    /// after it lands; until then the old snapshot drives nothing.
    var awaitingRead = false
    var pairWindow: PairWindow?

    override init() {
        super.init()
        menu.delegate = self
        item.menu = menu
        // Install the companion this app carries before anything reads it.
        inProgress = "Setting up the companion"
        build()
        coordinator.change({
            let outcome = ensureCompanion(install) { runCLI($0, $1, timeout: 40) }
            var notes = [outcome.note].compactMap { $0 }
            if case .installed = outcome {
                // Only right after a fresh install; afterwards the owner's choice stands.
                if let failed = LoginItem.set(true) { notes.append(failed) }
            }
            DispatchQueue.main.async { self.installNote = notes.isEmpty ? nil : notes.joined(separator: " · ") }
            if case .failed = outcome { return false }
            return true
        }) { _ in
            DispatchQueue.main.async {
                if !self.coordinator.busy { self.inProgress = nil }
                self.build()
                self.refresh()
            }
        }
        timer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in self?.refresh() }
    }

    // One refresh at a time; a result read before or during a change is dropped.
    func refresh() {
        guard !refreshing else { return }
        refreshing = true
        let generation = coordinator.beginObservation()
        refreshQueue.async {
            let status = runCLI(companion, ["status", "--json"], timeout: 8)
            let desktop = runCLI(companion, ["desktop", "status", "--json"], timeout: 8)
            let next = makeSnapshot(status: status, desktop: desktop, clock: clock)
            DispatchQueue.main.async {
                self.refreshing = false
                if self.coordinator.isCurrent(generation) {
                    self.snapshot = next
                    self.awaitingRead = false
                    self.build()
                }
            }
        }
    }

    func menuWillOpen(_ menu: NSMenu) { refresh() }

    func build() {
        let on: Bool
        if case .on = snapshot.desktop { on = true } else { on = false }
        let mark: HouseMark = on ? .on : (snapshot.desktop == .unknown ? .unknown : .off)
        item.button?.title = ""
        item.button?.image = houseMark(mark)
        // No tooltip: build() runs on every 5-second refresh, and re-setting a
        // status item's toolTip left it stuck to the owner's cursor. The image's
        // accessibilityDescription still names the state for VoiceOver.
        menu.removeAllItems()
        if let installNote {
            menu.addItem(disabled(installNote))
        }
        if !snapshot.companionReachable && notPaired() {
            menu.addItem(disabled("This Mac isn't paired yet"))
            menu.addItem(action("Pair this Mac…", #selector(pairThisMac)))
        } else if !snapshot.companionReachable {
            menu.addItem(disabled("Companion not answering"))
        }
        switch snapshot.desktop {
        case let .on(until, untilOff):
            if untilOff {
                menu.addItem(disabled("Desktop control: on until turned off"))
            } else if let until {
                menu.addItem(disabled("Desktop control: on until \(clock.string(from: until))"))
            }
        case .off:
            menu.addItem(disabled("Desktop control: off"))
        case .unknown:
            menu.addItem(disabled("Desktop control: unknown"))
        }
        if let inProgress {
            menu.addItem(disabled("\(inProgress)…"))
        } else {
            switch snapshot.desktop {
            case .on, .unknown:
                // In doubt, offer only the safe direction.
                menu.addItem(action("Turn off", #selector(turnOff)))
            case .off:
                menu.addItem(action("Turn on for 15 minutes", #selector(on15)))
                menu.addItem(action("Turn on for 1 hour", #selector(on60)))
                menu.addItem(action("Turn on until I turn it off", #selector(onUntilOff)))
            }
        }
        if let failedAction {
            menu.addItem(disabled("Could not \(failedAction)"))
        }
        menu.addItem(.separator())
        switch snapshot.trusted {
        case .some(true):
            menu.addItem(disabled("Accessibility: granted"))
        case .some(false):
            menu.addItem(disabled("Accessibility: needs you"))
            menu.addItem(action("Ask for Accessibility", #selector(requestAccessibility)))
            menu.addItem(action("Open Accessibility settings…", #selector(openAccessibility)))
        case .none:
            menu.addItem(disabled("Accessibility: not known"))
            menu.addItem(action("Open Accessibility settings…", #selector(openAccessibility)))
        }
        menu.addItem(.separator())
        // Run commands: the Mac's own half of the permission. The house's
        // Run commands switch is per resident; this is the local opt-in that
        // the companion checks before running anything. What is offered is
        // decided in MenuCore (lifecycleMenuItems) so it can be tested.
        let items = lifecycleMenuItems(executeAccepted: snapshot.executeAccepted, paused: snapshot.paused,
                                       busy: inProgress != nil, awaitingRead: awaitingRead,
                                       hold: hold, note: lifecycleNote)
        for entry in items {
            switch entry {
            case let .runCommands(checked, enabled):
                let i = NSMenuItem(title: entry.title, action: enabled ? #selector(toggleRunCommands) : nil,
                                   keyEquivalent: "")
                i.target = self
                i.state = checked ? .on : .off
                i.isEnabled = enabled
                menu.addItem(i)
            case .note:
                menu.addItem(disabled(entry.title))
            case .tryTurnOffAgain, .turnOffInstead:
                menu.addItem(action(entry.title, #selector(resolveByTurningOff)))
            case .checkAgain:
                menu.addItem(action(entry.title, #selector(resolveByChecking)))
            case .pauseOrResume:
                menu.addItem(.separator())
                menu.addItem(action(entry.title, #selector(togglePause)))
            }
        }
        if let last = snapshot.lastAction {
            menu.addItem(.separator())
            menu.addItem(disabled(last))
        }
        menu.addItem(.separator())
        let login = LoginItem.current
        let loginItem = action("Open at login", #selector(toggleLoginItem))
        loginItem.state = login == .on ? .on : .off
        menu.addItem(loginItem)
        if login == .needsApproval {
            menu.addItem(action("Allow in Login Items settings…", #selector(openLoginItems)))
        }
        menu.addItem(action("Quit menu", #selector(quit)))
    }

    /// No enrolment state on disk yet: the companion is installed but has
    /// never been paired, which is expected right after setup.
    func notPaired() -> Bool {
        let state = (home as NSString).appendingPathComponent(".local/share/souls-house-companion/state.json")
        return !FileManager.default.fileExists(atPath: state)
    }

    @objc func pairThisMac() {
        if pairWindow == nil {
            pairWindow = PairWindow(companion: companion) { [weak self] in
                self?.build()
                self?.refresh()
            }
        }
        pairWindow?.show()
    }
    @objc func toggleLoginItem() {
        installNote = LoginItem.set(LoginItem.current != .on)
        build()
    }
    @objc func openLoginItems() { SMAppService.openSystemSettingsLoginItems() }

    func disabled(_ title: String) -> NSMenuItem {
        let i = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        i.isEnabled = false
        return i
    }

    func action(_ title: String, _ selector: Selector) -> NSMenuItem {
        let i = NSMenuItem(title: title, action: selector, keyEquivalent: "")
        i.target = self
        return i
    }

    /// Changes run one at a time, in the order asked for.
    func change(_ label: String, _ args: [String]) {
        inProgress = label
        failedAction = nil
        build()
        coordinator.change({ runCLI(companion, args, timeout: 40).succeeded }) { ok in
            DispatchQueue.main.async {
                if !self.coordinator.busy { self.inProgress = nil }
                if !ok { self.failedAction = label.lowercased() }
                self.build()
                self.refresh()
            }
        }
    }

    @objc func on15() { change("Turn on for 15 minutes", ["desktop", "on", "--minutes", "15"]) }
    @objc func on60() { change("Turn on for 1 hour", ["desktop", "on", "--minutes", "60"]) }
    @objc func onUntilOff() { change("Turn on until turned off", ["desktop", "on", "--until-off"]) }
    @objc func turnOff() { change("Turn off", ["desktop", "off"]) }
    @objc func togglePause() {
        guard hold == nil else { return }
        let resume = snapshot.paused == true
        change(resume ? "Resume companion" : "Pause companion", [resume ? "resume" : "pause"])
    }
    @objc func requestAccessibility() { change("Ask for Accessibility", ["accessibility", "--request"]) }

    @objc func toggleRunCommands() {
        guard hold == nil else { return }
        let allow = snapshot.executeAccepted != true
        if allow && !confirmRunCommands() { return }
        runLifecycle(allow ? "Allowing commands" : "Turning commands off", allow: allow) { run in
            setExecuteLifecycle(allow: allow, run: run)
        }
    }

    @objc func resolveByTurningOff() {
        guard let current = hold else { return }
        runLifecycle("Turning commands off", allow: false) { run in resolveHold(current, turnOff: true, run: run) }
    }

    @objc func resolveByChecking() {
        guard let current = hold else { return }
        runLifecycle("Checking", allow: true) { run in resolveHold(current, turnOff: false, run: run) }
    }

    /// Runs one run-commands change, then waits for a fresh status read
    /// before the switch or the pause item can be used again.
    func runLifecycle(_ label: String, allow: Bool, _ body: @escaping (([String]) -> CLIResult) -> LifecycleChange) {
        inProgress = label
        failedAction = nil
        lifecycleNote = nil
        build()
        var outcome = LifecycleChange.couldNotRead
        coordinator.change({
            outcome = body { runCLI(companion, $0, timeout: 40) }
            return outcome == .done
        }) { _ in
            DispatchQueue.main.async {
                if !self.coordinator.busy { self.inProgress = nil }
                self.hold = outcome.hold
                self.lifecycleNote = lifecycleMessage(outcome, allow: allow)
                self.awaitingRead = true
                self.build()
                self.refresh()
            }
        }
    }

    /// Asked here, at the Mac, every time it is turned on. Nothing a
    /// resident can send reaches this dialog.
    func confirmRunCommands() -> Bool {
        NSApp.activate(ignoringOtherApps: true)
        let alert = NSAlert()
        alert.messageText = "Let residents run commands on this Mac?"
        alert.informativeText = """
            Residents you have given Run commands on your Computers page will be able to run \
            commands on this Mac as you, with your user's access.

            macOS can't guarantee that every process a command starts is stopped afterwards. \
            You can turn this off here at any time.
            """
        alert.addButton(withTitle: "Allow commands")
        alert.addButton(withTitle: "Cancel")
        return alert.runModal() == .alertFirstButtonReturn
    }

    @objc func openAccessibility() { NSWorkspace.shared.open(accessibilityPane) }
    @objc func quit() { NSApp.terminate(nil) }
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory)
if mustBeInApplications() { exit(0) }
if mustLeaveEarlierSetup() { exit(0) }
if install.needsFirstInstall && !confirmFirstInstall() { exit(0) }
let controller = Menu()
app.run()
