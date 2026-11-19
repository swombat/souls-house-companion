// Installing from the app itself: the download is a disk image with this
// app in it, and the rest (the companion, its LaunchAgent, opening at login)
// is done here after one question, so nobody needs Terminal.
//
// This release installs fresh only. A Mac set up by an earlier release is
// left exactly as it is; updating one gets its own, separately reviewed path.
import AppKit
import MenuCore
import ServiceManagement

let home = NSHomeDirectory()
let legacyMenuAgent = (home as NSString).appendingPathComponent("Library/LaunchAgents/house.souls.menu.plist")

/// The companion this app carries, next to the menu's own executable.
let install = CompanionInstall(
    bundled: Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/souls-house-companion").path,
    home: home)

func inApplications(_ path: String) -> Bool {
    path.hasPrefix("/Applications/") || path.hasPrefix((home as NSString).appendingPathComponent("Applications") + "/")
}

func tell(_ title: String, _ text: String) {
    NSApp.activate(ignoringOtherApps: true)
    let alert = NSAlert()
    alert.messageText = title
    alert.informativeText = text
    alert.addButton(withTitle: "OK")
    alert.runModal()
}

/// Opened from the disk image or Downloads: Finder does the copying.
func mustBeInApplications() -> Bool {
    guard !inApplications(Bundle.main.bundlePath) else { return false }
    tell("Drag souls.house to Applications first",
         "In the souls.house window, drag the souls.house icon onto the Applications folder, then open it from Applications.")
    return true
}

/// A Mac set up by an earlier release keeps its working setup untouched.
func mustLeaveEarlierSetup() -> Bool {
    guard earlierSetup(install, legacyMenuAgent: legacyMenuAgent, run: { runCLI($0, $1, timeout: 15) }) else {
        return false
    }
    tell("souls.house is already set up on this Mac",
         "It was set up by an earlier version, which keeps working as it is. This version only sets up Macs that don't have it yet; updating comes in the next version.")
    return true
}

/// Asked once, before the first install touches anything.
func confirmFirstInstall() -> Bool {
    NSApp.activate(ignoringOtherApps: true)
    let alert = NSAlert()
    alert.messageText = "Set up souls.house on this Mac?"
    alert.informativeText = """
        This puts the souls.house companion in your user folder and opens souls.house \
        when you log in. It doesn't need your password and doesn't connect anywhere \
        until you pair this Mac from souls.house.
        """
    alert.addButton(withTitle: "Set up")
    alert.addButton(withTitle: "Quit")
    return alert.runModal() == .alertFirstButtonReturn
}

enum LoginItem {
    case on, off, needsApproval

    static var current: LoginItem {
        switch SMAppService.mainApp.status {
        case .enabled: return .on
        case .requiresApproval: return .needsApproval
        default: return .off
        }
    }

    /// Returns nil on success, or what to show in the menu.
    @discardableResult
    static func set(_ on: Bool) -> String? {
        do {
            if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
            return nil
        } catch {
            return on ? "Couldn't set souls.house to open at login" : "Couldn't stop souls.house opening at login"
        }
    }
}
