//go:build darwin

package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestDarwinPlistEscapingAndLifecycle(t *testing.T) {
	data, err := launchAgentPlist("/Users/fixture/A&B/<binary>", "/Users/fixture/state & ceiling")
	if err != nil {
		t.Fatal(err)
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	for {
		_, err = d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, part := range []string{"A&amp;B", "&lt;binary&gt;", "SuccessfulExit", "ThrottleInterval", "ExitTimeOut"} {
		if !strings.Contains(string(data), part) {
			t.Fatalf("missing %s", part)
		}
	}
	if _, err := launchAgentPlist("relative", "/absolute"); err == nil {
		t.Fatal("relative executable allowed")
	}
	if _, err := launchAgentPlist("/binary", "/line\nbreak"); err == nil {
		t.Fatal("newline allowed")
	}
}
func TestDarwinManagerOutputBound(t *testing.T) {
	b := &limitedManagerOutput{}
	n, err := b.Write(make([]byte, (1<<20)+3))
	if err != nil || n != (1<<20)+3 || b.Len() != 1<<20 || !b.overflow {
		t.Fatal("unbounded manager output")
	}
}

func TestDarwinOwnerDisabledParsing(t *testing.T) {
	for _, input := range []string{
		"disabled services = {\n \"house.souls.companion\" => true\n}",
		"disabled services = {\n\t\"house.souls.companion\" =>   true;\n}",
	} {
		if disabled, err := ownerDisabled([]byte(input)); err != nil || !disabled {
			t.Fatal("owner disable ignored")
		}
	}
	if _, err := ownerDisabled([]byte("unknown output")); err == nil {
		t.Fatal("unknown output reported enabled")
	}
}
