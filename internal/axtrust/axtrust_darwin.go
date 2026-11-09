//go:build darwin

package axtrust

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ebitengine/purego"
)

// Supported reports whether this platform has macOS Accessibility trust.
const Supported = true

// The prompt option is built from a property list so no CoreFoundation data
// symbols (kCFBooleanTrue, kAXTrustedCheckOptionPrompt) need dereferencing.
var promptOptions = []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>AXTrustedCheckOptionPrompt</key><true/></dict></plist>
`)

var (
	loadOnce        sync.Once
	loadErr         error
	axIsTrusted     func() bool
	axIsTrustedWith func(options uintptr) bool
	cfDataCreate    func(allocator uintptr, bytes *byte, length int) uintptr
	cfPlistCreate   func(allocator, data, options, format, err uintptr) uintptr
	cfRelease       func(ref uintptr)
)

func load() (err error) {
	defer func() { // RegisterLibFunc panics on a missing symbol
		if r := recover(); r != nil {
			err = fmt.Errorf("accessibility symbols unavailable")
		}
	}()
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	as, err := purego.Dlopen("/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	purego.RegisterLibFunc(&cfDataCreate, cf, "CFDataCreate")
	purego.RegisterLibFunc(&cfPlistCreate, cf, "CFPropertyListCreateWithData")
	purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
	purego.RegisterLibFunc(&axIsTrusted, as, "AXIsProcessTrusted")
	purego.RegisterLibFunc(&axIsTrustedWith, as, "AXIsProcessTrustedWithOptions")
	return nil
}

// System asks TCC about the calling process. With prompt, macOS adds the
// process to the Accessibility list and shows its own dialog if untrusted.
func System(prompt bool) (bool, error) {
	loadOnce.Do(func() { loadErr = load() })
	if loadErr != nil {
		return false, loadErr
	}
	if !prompt {
		return axIsTrusted(), nil
	}
	data := cfDataCreate(0, &promptOptions[0], len(promptOptions))
	if data == 0 {
		return false, errors.New("options data unavailable")
	}
	defer cfRelease(data)
	options := cfPlistCreate(0, data, 0, 0, 0)
	if options == 0 {
		return false, errors.New("options unavailable")
	}
	defer cfRelease(options)
	return axIsTrustedWith(options), nil
}
