// Installing from the app itself: the download is a disk image with this
// app in it, and everything else (the companion, its LaunchAgent, opening at
// login) is done here, after one question, so nobody needs Terminal.
import AppKit
import MenuCore
import ServiceManagement

let menuLabel = "house.souls.menu"
let home = NSHomeDirectory()
let legacyMenuAgent = (home as NSString).appendingPathComponent("Library/LaunchAgents/\(menuLabel).plist")

/// The companion this app carries, next to the menu's own executable.
var install: CompanionInstall {
    let bundled = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/souls-house-companion").path
    return CompanionInstall(bundled: bundled, home: home)
}

/// The version this process was started from, to notice the app being replaced on disk.
let launchedVersion = Bundle.main.object(forInfoDictionaryKey: "CFBundleVersion") as? String

func inApplications(_ path: String) -> Bool {
    path.hasPrefix("/Applications/") || path.hasPrefix((home as NSString).appendingPathComponent("Applications") + "/")
}

/// Opened from the disk image or Downloads: offer to move it to Applications,
/// then open that copy. Returns true when this process should quit.
func moveToApplicationsIfNeeded() -> Bool {
    let here = Bundle.main.bundleURL
    guard !inApplications(here.path) else { return false }
    NSApp.activate(ignoringOtherApps: true)
    let alert = NSAlert()
    alert.messageText = "Move souls.house to your Applications folder?"
    alert.informativeText = "It runs from there, and opens itself when you log in."
    alert.addButton(withTitle: "Move to Applications")
    alert.addButton(withTitle: "Quit")
    guard alert.runModal() == .alertFirstButtonReturn else { return true }
    let fm = FileManager.default
    let name = here.lastPathComponent
    for folder in ["/Applications", (home as NSString).appendingPathComponent("Applications")] {
        let dest = URL(fileURLWithPath: folder).appendingPathComponent(name)
        do {
            try fm.createDirectory(atPath: folder, withIntermediateDirectories: true)
            if fm.fileExists(atPath: dest.path) { try fm.trashItem(at: dest, resultingItemURL: nil) }
            try fm.copyItem(at: here, to: dest)
            relaunch(dest)
            return true
        } catch {
            continue
        }
    }
    let failed = NSAlert()
    failed.messageText = "Couldn't move souls.house"
    failed.informativeText = "Drag it into your Applications folder in Finder, then open it from there."
    failed.runModal()
    return true
}

/// Opens a fresh instance of the app at `url`. The caller quits.
func relaunch(_ url: URL) {
    let config = NSWorkspace.OpenConfiguration()
    config.createsNewApplicationInstance = true
    let done = DispatchSemaphore(value: 0)
    NSWorkspace.shared.openApplication(at: url, configuration: config) { _, _ in done.signal() }
    _ = done.wait(timeout: .now() + 10)
}

/// Earlier releases started the menu with their own LaunchAgent and kept the
/// app in ~/Applications/SoulsHouseMenu.app. This app opens at login through
/// macOS Login Items instead, so the old agent and any other running copy go.
func retireOlderMenus() {
    let me = ProcessInfo.processInfo
    let launchedByOldAgent = me.environment["XPC_SERVICE_NAME"] == menuLabel
    if FileManager.default.fileExists(atPath: legacyMenuAgent) {
        try? FileManager.default.trashItem(at: URL(fileURLWithPath: legacyMenuAgent), resultingItemURL: nil)
        if !launchedByOldAgent {
            // Unloading the agent stops the copy it started; never our own.
            _ = runCLI("/bin/launchctl", ["bootout", "gui/\(getuid())/\(menuLabel)"], timeout: 10)
        }
    }
    for other in NSRunningApplication.runningApplications(withBundleIdentifier: menuLabel)
    where other.processIdentifier != me.processIdentifier {
        other.terminate()
    }
    let old = (home as NSString).appendingPathComponent("Applications/SoulsHouseMenu.app")
    if Bundle.main.bundlePath != old, FileManager.default.fileExists(atPath: old) {
        try? FileManager.default.trashItem(at: URL(fileURLWithPath: old), resultingItemURL: nil)
    }
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

    /// Registers once, the first time this app is set up; after that the
    /// owner's own choice in the menu (or System Settings) stands.
    static func registerOnFirstRun() {
        let key = "loginItemOffered"
        guard !UserDefaults.standard.bool(forKey: key) else { return }
        UserDefaults.standard.set(true, forKey: key)
        try? SMAppService.mainApp.register()
    }

    static func set(_ on: Bool) {
        if on { try? SMAppService.mainApp.register() } else { try? SMAppService.mainApp.unregister() }
    }
}

/// True when the app on disk has been replaced by a different build (an
/// update dragged over it); the running copy then opens the new one and quits.
func replacedOnDisk() -> Bool {
    let plist = Bundle.main.bundleURL.appendingPathComponent("Contents/Info.plist")
    guard let info = NSDictionary(contentsOf: plist), let onDisk = info["CFBundleVersion"] as? String else {
        return false
    }
    return launchedVersion != nil && onDisk != launchedVersion
}
