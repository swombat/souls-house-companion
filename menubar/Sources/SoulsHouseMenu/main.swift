// The souls.house menu bar: the owner's switch for desktop control.
//
// It holds no credentials and talks to nothing but the installed companion
// CLI. Every authority decision (including expiry) is enforced by the
// companion; if this app quits or crashes, control still turns itself off.
import AppKit

let companion = (NSHomeDirectory() as NSString).appendingPathComponent(".local/bin/souls-house-companion")
let accessibilityPane = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility")!

struct CLIResult {
    let status: Int32
    let output: Data
}

func runCompanion(_ args: [String], timeout: TimeInterval = 10) -> CLIResult {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: companion)
    process.arguments = args
    let pipe = Pipe()
    process.standardOutput = pipe
    process.standardError = FileHandle.nullDevice
    do { try process.run() } catch { return CLIResult(status: -1, output: Data()) }
    let deadline = Date().addingTimeInterval(timeout)
    var data = Data()
    let reader = pipe.fileHandleForReading
    while process.isRunning && Date() < deadline {
        data.append(reader.availableData)
        if data.count > 1 << 20 { break }
    }
    if process.isRunning { process.terminate() }
    process.waitUntilExit()
    data.append(reader.readDataToEndOfFile())
    return CLIResult(status: process.terminationStatus, output: data)
}

func json(_ data: Data) -> [String: Any]? {
    (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
}

let iso: ISO8601DateFormatter = {
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return f
}()

func parseDate(_ value: Any?) -> Date? {
    guard let s = value as? String else { return nil }
    if let d = iso.date(from: s) { return d }
    return ISO8601DateFormatter().date(from: s)
}

let clock: DateFormatter = {
    let f = DateFormatter()
    f.dateFormat = "HH:mm"
    return f
}()

final class Menu: NSObject, NSMenuDelegate {
    let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    let menu = NSMenu()
    var timer: Timer?

    var desktopOn = false
    var desktopUntil: Date?
    var desktopUntilOff = false
    var paused = false
    var trusted: Bool?
    var lastAction: String?
    var reachable = true

    override init() {
        super.init()
        menu.delegate = self
        item.menu = menu
        refresh()
        timer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in self?.refresh() }
    }

    func refresh() {
        DispatchQueue.global(qos: .utility).async {
            let status = runCompanion(["status", "--json"])
            let desktop = runCompanion(["desktop", "status", "--json"])
            DispatchQueue.main.async { self.apply(status: status, desktop: desktop) }
        }
    }

    func apply(status: CLIResult, desktop: CLIResult) {
        reachable = status.status == 0
        if let s = json(status.output) {
            paused = (s["paused"] as? Bool) ?? false
            if let ax = s["accessibility"] as? [String: Any] {
                trusted = ax["trusted"] as? Bool
            } else {
                trusted = nil
            }
        }
        if desktop.status == 0, let d = json(desktop.output) {
            desktopOn = (d["on"] as? Bool) ?? false
            desktopUntilOff = (d["until_off"] as? Bool) ?? false
            desktopUntil = parseDate(d["until"])
            if let last = d["last_action"] as? [String: Any],
               let app = last["app"] as? String, let command = last["command"] as? String {
                let who = (last["resident"] as? String) ?? "?"
                let when = parseDate(last["time"]).map { clock.string(from: $0) } ?? ""
                lastAction = "Last: \(who) · \(command) in \(app) · \(when)"
            }
        } else {
            desktopOn = false
        }
        item.button?.title = desktopOn ? "⌂●" : "⌂"
        item.button?.toolTip = desktopOn ? "souls.house: desktop control on" : "souls.house: desktop control off"
        build()
    }

    func menuWillOpen(_ menu: NSMenu) { refresh() }

    func build() {
        menu.removeAllItems()
        if !reachable {
            menu.addItem(disabled("Companion not reachable"))
        }
        let state: String
        if desktopOn {
            if desktopUntilOff {
                state = "Desktop control: on until turned off"
            } else if let until = desktopUntil {
                state = "Desktop control: on until \(clock.string(from: until))"
            } else {
                state = "Desktop control: on"
            }
        } else {
            state = "Desktop control: off"
        }
        menu.addItem(disabled(state))
        if desktopOn {
            menu.addItem(action("Turn off", #selector(turnOff)))
        } else {
            menu.addItem(action("Turn on for 15 minutes", #selector(on15)))
            menu.addItem(action("Turn on for 1 hour", #selector(on60)))
            menu.addItem(action("Turn on until I turn it off", #selector(onUntilOff)))
        }
        menu.addItem(.separator())
        switch trusted {
        case .some(true):
            menu.addItem(disabled("Accessibility: granted"))
        case .some(false):
            menu.addItem(disabled("Accessibility: needs you"))
            menu.addItem(action("Ask for Accessibility", #selector(requestAccessibility)))
            menu.addItem(action("Open Accessibility settings…", #selector(openAccessibility)))
        case .none:
            menu.addItem(disabled("Accessibility: not checked yet"))
            menu.addItem(action("Open Accessibility settings…", #selector(openAccessibility)))
        }
        menu.addItem(.separator())
        menu.addItem(action(paused ? "Resume companion" : "Pause companion", #selector(togglePause)))
        if let last = lastAction {
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

    func run(_ args: [String]) {
        DispatchQueue.global(qos: .userInitiated).async {
            _ = runCompanion(args, timeout: 40)
            DispatchQueue.main.async { self.refresh() }
        }
    }

    @objc func on15() { run(["desktop", "on", "--minutes", "15"]) }
    @objc func on60() { run(["desktop", "on", "--minutes", "60"]) }
    @objc func onUntilOff() { run(["desktop", "on", "--until-off"]) }
    @objc func turnOff() { run(["desktop", "off"]) }
    @objc func togglePause() { run([paused ? "resume" : "pause"]) }
    @objc func requestAccessibility() { run(["accessibility", "--request"]) }
    @objc func openAccessibility() { NSWorkspace.shared.open(accessibilityPane) }
    @objc func quit() { NSApp.terminate(nil) }
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory)
let controller = Menu()
app.run()
