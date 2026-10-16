package policy

import "testing"

func grant(access Access, execute bool) Policy {
	return Policy{Residents: []Resident{{ID: "mira", Volumes: []Grant{{Volume: "volume-a", Access: access}}, Execute: execute}}}
}

func TestIntersection(t *testing.T) {
	for _, tt := range []struct {
		name              string
		local             Policy
		remote            Policy
		read, write, exec bool
	}{
		{"empty", Policy{}, Policy{}, false, false, false},
		{"read", grant(Read, false), grant(Read, false), true, false, false},
		{"remote cannot widen", grant(Read, false), grant(Write, true), true, false, false},
		{"remote narrows", grant(Write, true), grant(Read, false), true, false, false},
		{"both write", grant(Write, true), grant(Write, true), true, true, true},
		{"revoked remote", grant(Write, true), Policy{}, false, false, false},
		{"revoked local", Policy{}, grant(Write, true), false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Allows(tt.local, tt.remote, "mira", "volume-a", Read); got != tt.read {
				t.Errorf("read = %v", got)
			}
			if got := Allows(tt.local, tt.remote, "mira", "volume-a", Write); got != tt.write {
				t.Errorf("write = %v", got)
			}
			if got := AllowsExecute(tt.local, tt.remote, "mira"); got != tt.exec {
				t.Errorf("execute = %v", got)
			}
			if Allows(tt.local, tt.remote, "lume", "volume-a", Read) ||
				Allows(tt.local, tt.remote, "mira", "volume-other", Read) ||
				Allows(tt.local, tt.remote, "mira", "volume-a", Access("unknown")) {
				t.Error("ungranted actor/volume/access permitted")
			}
		})
	}
}

func TestMalformedPolicyFailsClosed(t *testing.T) {
	cases := []Policy{
		{Residents: []Resident{{ID: ""}}},
		{Residents: []Resident{{ID: "mira"}, {ID: "mira"}}},
		grant(Access("admin"), true),
		{Residents: []Resident{{ID: "mira", Volumes: []Grant{{Volume: ""}}}}},
		{Residents: []Resident{{ID: "mira", Volumes: []Grant{{Volume: "volume-a", Access: Read}, {Volume: "volume-a", Access: Write}}}}},
	}
	for _, bad := range cases {
		if bad.Validate() == nil {
			t.Fatal("malformed policy accepted")
		}
		for _, pair := range [][2]Policy{{bad, grant(Write, true)}, {grant(Write, true), bad}} {
			if Allows(pair[0], pair[1], "mira", "volume-a", Read) || AllowsExecute(pair[0], pair[1], "mira") {
				t.Fatal("malformed policy permitted access")
			}
		}
	}
}

func TestMoveRequiresSourceWrite(t *testing.T) {
	p := Policy{Residents: []Resident{{ID: "mira", Volumes: []Grant{
		{Volume: "source", Access: Read}, {Volume: "destination", Access: Write},
	}}}}
	if AllowsMove(p, p, "mira", "source", "destination") {
		t.Fatal("move permitted from read-only source")
	}
	if !Allows(p, p, "mira", "source", Read) || !Allows(p, p, "mira", "destination", Write) {
		t.Fatal("copy requirements not permitted")
	}
	p.Residents[0].Volumes[0].Access = Write
	if !AllowsMove(p, p, "mira", "source", "destination") {
		t.Fatal("move with both writes denied")
	}
}
