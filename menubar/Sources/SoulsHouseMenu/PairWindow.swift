// "Pair this Mac…": a small native window. Step 1 chooses what this Mac
// offers, step 2 shows the code to type on souls.house. The command runs in
// MenuCore (PairingRun); this file only draws what it reports.
import AppKit
import MenuCore

let houseURL = "https://souls.house"
let computersPage = URL(string: "https://souls.house/computers")!

final class PairWindow: NSObject, NSWindowDelegate {
    private let companionPath: String
    private let onPaired: () -> Void
    private let window: NSWindow
    private let stack = NSStackView()
    private var boxes: [(volume: PairVolume, button: NSButton)] = []
    private let access = NSPopUpButton(frame: .zero, pullsDown: false)
    private var run: PairingRun?
    private let contentWidth: CGFloat = 440

    init(companion: String, onPaired: @escaping () -> Void) {
        companionPath = companion
        self.onPaired = onPaired
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 488, height: 300),
                          styleMask: [.titled, .closable], backing: .buffered, defer: false)
        super.init()
        window.title = "Pair this Mac"
        window.isReleasedWhenClosed = false
        window.delegate = self
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 12
        stack.translatesAutoresizingMaskIntoConstraints = false
        let container = NSView()
        container.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.topAnchor.constraint(equalTo: container.topAnchor, constant: 24),
            stack.leadingAnchor.constraint(equalTo: container.leadingAnchor, constant: 24),
            stack.widthAnchor.constraint(equalToConstant: contentWidth),
            container.bottomAnchor.constraint(equalTo: stack.bottomAnchor, constant: 24),
            container.trailingAnchor.constraint(equalTo: stack.trailingAnchor, constant: 24),
        ])
        window.contentView = container
        access.addItems(withTitles: ["Read only", "Read and write"])
    }

    /// Opens at step 1 (or brings an open window forward).
    func show() {
        if !window.isVisible {
            show(message: "Looking at this Mac's disks…", buttons: [])
            window.center()
            let path = companionPath
            DispatchQueue.global().async {
                let volumes = listVolumes(companion: path) { runCLI($0, $1, timeout: 15) }
                DispatchQueue.main.async { self.showChoices(volumes) }
            }
        }
        NSApp.activate(ignoringOtherApps: true)
        window.makeKeyAndOrderFront(nil)
    }

    func windowWillClose(_ notification: Notification) { stop() }

    private func stop() {
        run?.cancel()
        run = nil
    }

    // MARK: building blocks

    private func title(_ text: String) -> NSTextField {
        let f = NSTextField(wrappingLabelWithString: text)
        f.font = .boldSystemFont(ofSize: 17)
        f.preferredMaxLayoutWidth = contentWidth
        return f
    }

    private func text(_ text: String, secondary: Bool = false) -> NSTextField {
        let f = NSTextField(wrappingLabelWithString: text)
        f.font = .systemFont(ofSize: secondary ? 12 : 13)
        f.textColor = secondary ? .secondaryLabelColor : .labelColor
        f.isSelectable = true
        f.preferredMaxLayoutWidth = contentWidth
        return f
    }

    private func button(_ title: String, _ selector: Selector, primary: Bool = false) -> NSButton {
        let b = NSButton(title: title, target: self, action: selector)
        b.bezelStyle = .rounded
        if primary { b.keyEquivalent = "\r" }
        return b
    }

    private func row(_ buttons: [NSButton]) -> NSView {
        let r = NSStackView(views: buttons)
        r.orientation = .horizontal
        r.spacing = 12
        return r
    }

    /// Replaces what the window shows and fits the window to it.
    private func set(_ views: [NSView]) {
        for v in stack.arrangedSubviews { stack.removeArrangedSubview(v); v.removeFromSuperview() }
        for v in views { stack.addArrangedSubview(v) }
        window.contentView?.layoutSubtreeIfNeeded()
        if let fit = window.contentView?.fittingSize {
            window.setContentSize(NSSize(width: 488, height: fit.height))
        }
    }

    private func show(message: String, buttons: [NSButton]) {
        var views: [NSView] = [title("Pair this Mac with souls.house"), text(message)]
        if !buttons.isEmpty { views.append(row(buttons)) }
        set(views)
    }

    // MARK: step 1

    private func showChoices(_ volumes: [PairVolume]?) {
        guard let volumes else {
            show(message: "souls.house couldn't read this Mac's disks. Quit souls.house from its menu, open it again and retry.",
                 buttons: [button("Try again", #selector(retry)), button("Close", #selector(closeWindow))])
            return
        }
        guard !volumes.isEmpty else {
            show(message: "This Mac has no disk that souls.house can be given. Nothing was shared.",
                 buttons: [button("Close", #selector(closeWindow))])
            return
        }
        boxes = volumes.map { v in
            let b = NSButton(checkboxWithTitle: v.label, target: self, action: #selector(choiceChanged))
            b.state = v.isMain ? .on : .off
            return (v, b)
        }
        let accessRow = NSStackView(views: [text("Access:"), access])
        accessRow.orientation = .horizontal
        accessRow.spacing = 8
        var views: [NSView] = [title("Pair this Mac with souls.house"),
                               text("Choose what this Mac can share.")]
        views += boxes.map { $0.button }
        views += [accessRow,
                  text("souls.house can never reach more than this. You choose which residents get it on the website.",
                       secondary: true),
                  row([button("Cancel", #selector(closeWindow)),
                       button("Get a pairing code", #selector(getCode), primary: true)])]
        set(views)
        choiceChanged()
    }

    @objc private func choiceChanged() {
        // The last row of the stack is the buttons; the code button needs a disk ticked.
        if let buttons = stack.arrangedSubviews.last as? NSStackView,
           let go = buttons.arrangedSubviews.last as? NSButton {
            go.isEnabled = boxes.contains { $0.button.state == .on }
        }
    }

    // MARK: step 2

    @objc private func getCode() {
        let chosen = boxes.filter { $0.button.state == .on }.map {
            PairChoice(key: $0.volume.key, write: access.indexOfSelectedItem == 1)
        }
        guard !chosen.isEmpty else { return }
        let args = pairArguments(url: houseURL, name: pairDeviceName(Host.current().localizedName), choices: chosen)
        show(message: "Getting your pairing code…", buttons: [button("Cancel", #selector(closeWindow))])
        let next = PairingRun(path: companionPath, args: args)
        run = next
        next.start { [weak self, weak next] state in
            DispatchQueue.main.async {
                guard let self, let next, self.run === next else { return }
                self.render(state)
            }
        }
    }

    private func render(_ state: PairState) {
        switch state {
        case .waiting:
            break
        case let .code(code, _):
            let big = NSTextField(labelWithString: code)
            big.font = .monospacedSystemFont(ofSize: 34, weight: .semibold)
            big.isSelectable = true
            let copy = button("Copy", #selector(copyCode))
            copy.identifier = NSUserInterfaceItemIdentifier(code)
            set([title("Your pairing code"), big,
                 text("Click Open souls.house (or go to Settings → Computers → Add a computer there) and type this code. It works for 15 minutes, once."),
                 row([copy, button("Open souls.house", #selector(openSite))]),
                 row([button("Cancel", #selector(closeWindow))])])
        case .paired:
            run = nil
            show(message: "Paired. Starting the connection…", buttons: [])
            let path = companionPath
            DispatchQueue.global().async {
                let started = runCLI(path, ["resume"], timeout: 40).succeeded
                DispatchQueue.main.async { self.finishPairing(started) }
            }
        case .expired:
            run = nil
            show(message: "That code ran out of time. A code works for 15 minutes, and nothing was shared.",
                 buttons: [button("Try again", #selector(retry)), button("Close", #selector(closeWindow))])
        case .cancelled:
            run = nil
            show(message: "Pairing was cancelled. Nothing was shared.",
                 buttons: [button("Try again", #selector(retry)), button("Close", #selector(closeWindow))])
        case let .failed(detail):
            run = nil
            let more = detail.isEmpty ? "" : "\n\n\(detail)"
            show(message: "Pairing didn't work, and nothing was shared.\(more)",
                 buttons: [button("Try again", #selector(retry)), button("Close", #selector(closeWindow))])
        }
    }

    private func finishPairing(_ started: Bool) {
        onPaired()
        if started {
            window.close()
        } else {
            show(message: "This Mac is paired, but souls.house couldn't start the connection. Quit souls.house from its menu and open it again.",
                 buttons: [button("Close", #selector(closeWindow))])
        }
    }

    // MARK: buttons

    @objc private func retry() {
        stop()
        window.close()
        show()
    }

    @objc private func closeWindow() { window.close() }

    @objc private func copyCode(_ sender: NSButton) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(sender.identifier?.rawValue ?? "", forType: .string)
        sender.title = "Copied"
        DispatchQueue.main.asyncAfter(deadline: .now() + 2) { sender.title = "Copy" }
    }

    @objc private func openSite() { NSWorkspace.shared.open(computersPage) }
}
