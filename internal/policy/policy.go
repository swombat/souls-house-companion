// Package policy intersects independently approved local and remote grants.
// It does not identify volumes or authenticate remote policy updates.
package policy

import "errors"

var ErrDenied = errors.New("operation not permitted")

type Access string

const (
	Read  Access = "read"
	Write Access = "write"
)

// Grant names a volume identity, never a directory prefix. The volume resolver
// must independently validate its identity before using any filesystem handle.
type Grant struct {
	Volume string `json:"volume"`
	Access Access `json:"access"`
}

type Resident struct {
	ID      string  `json:"id"`
	Volumes []Grant `json:"volumes"`
	Execute bool    `json:"execute"`
}

type Policy struct {
	Residents []Resident `json:"residents"`
}

func (p Policy) Validate() error {
	if len(p.Residents) > 128 {
		return errors.New("too many residents")
	}
	seen := make(map[string]bool)
	for _, r := range p.Residents {
		if r.ID == "" || len(r.ID) > 128 || seen[r.ID] {
			return errors.New("empty, oversized or duplicate resident")
		}
		seen[r.ID] = true
		if len(r.Volumes) > 128 {
			return errors.New("too many volumes")
		}
		volumes := make(map[string]bool)
		for _, g := range r.Volumes {
			if g.Volume == "" || len(g.Volume) > 1024 || volumes[g.Volume] {
				return errors.New("empty, oversized or duplicate volume")
			}
			volumes[g.Volume] = true
			if g.Access != Read && g.Access != Write {
				return errors.New("unknown access")
			}
		}
	}
	return nil
}

// Allows refuses malformed policies in their entirety, not just malformed rows.
func Allows(local, remote Policy, resident, volume string, access Access) bool {
	if local.Validate() != nil || remote.Validate() != nil || resident == "" || volume == "" {
		return false
	}
	return allowsOne(local, resident, volume, access) && allowsOne(remote, resident, volume, access)
}

func allowsOne(p Policy, resident, volume string, access Access) bool {
	if access != Read && access != Write {
		return false
	}
	for _, r := range p.Residents {
		if r.ID != resident {
			continue
		}
		for _, g := range r.Volumes {
			if g.Volume == volume {
				return g.Access == Write || access == Read
			}
		}
	}
	return false
}

func AllowsExecute(local, remote Policy, resident string) bool {
	if local.Validate() != nil || remote.Validate() != nil || resident == "" {
		return false
	}
	return executes(local, resident) && executes(remote, resident)
}

func executes(p Policy, resident string) bool {
	for _, r := range p.Residents {
		if r.ID == resident {
			return r.Execute
		}
	}
	return false
}

// AllowsMove requires source mutation authority as well as destination write.
// This is intentionally stronger than copy, which only needs source read.
func AllowsMove(local, remote Policy, resident, source, destination string) bool {
	return Allows(local, remote, resident, source, Write) &&
		Allows(local, remote, resident, destination, Write)
}
