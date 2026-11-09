// The souls.house menu bar: the owner's switch for desktop control.
//
// It holds no credentials and talks to nothing but the installed companion
// CLI. Every authority decision (including expiry) is enforced by the
// companion; if this app quits or crashes, control still turns itself off.
import AppKit
import MenuCore

let companion = (NSHomeDirectory() as NSString).appendingPathComponent(".local/bin/souls-house-companion")
let accessibilityPane = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility")!

let clock: DateFormatter = {
    let f = DateFormatter()
    f.dateFormat = "HH:mm"
    return f
}()

final class Menu: NSObject, NSMenuDelegate {
    let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    let menu = NSMenu()
    let coordinator = Coordinator()
    let refreshQueue = DispatchQueue(label: "house.souls.menu.refresh")
    var refreshing = false
    var timer: Timer?
    var snapshot = Snapshot()
    var failedAction: String?
    var inProgress: String?

    override init() {
        super.init()
        menu.delegate = self
        item.menu = menu
        refresh()
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
                    self.build()
                }
            }
        }
    }

    func menuWillOpen(_ menu: NSMenu) { refresh() }

    func build() {
        let on: Bool
        if case .on = snapshot.desktop { on = true } else { on = false }
        item.button?.title = on ? "⌂●" : (snapshot.desktop == .unknown ? "⌂?" : "⌂")
        menu.removeAllItems()
        if !snapshot.companionReachable {
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
        if inProgress == nil, let paused = snapshot.paused {
            menu.addItem(action(paused ? "Resume companion" : "Pause companion", #selector(togglePause)))
        }
        if let last = snapshot.lastAction {
            menu.addItem(.separator())
            menu.addItem(disabled(last))
        }
        menu.addItem(.separator())
        menu.addItem(action("Quit menu", #selector(quit)))
    }

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
        let resume = snapshot.paused == true
        change(resume ? "Resume companion" : "Pause companion", [resume ? "resume" : "pause"])
    }
    @objc func requestAccessibility() { change("Ask for Accessibility", ["accessibility", "--request"]) }
    @objc func openAccessibility() { NSWorkspace.shared.open(accessibilityPane) }
    @objc func quit() { NSApp.terminate(nil) }
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory)
let controller = Menu()
app.run()
