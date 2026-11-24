package client

import (
	"encoding/json"
	"errors"

	"github.com/swombat/souls-house-companion/internal/policy"
)

type reportedVolume struct {
	Volume string        `json:"volume"`
	Access policy.Access `json:"access"`
}

type reportedResident struct {
	ID      string           `json:"id"`
	Execute bool             `json:"execute"`
	Volumes []reportedVolume `json:"volumes"`
}

// ceilingReportFrame builds the one Action Cable message that tells the house
// which volumes and command execution each enrolled resident's local ceiling
// allows. It carries volume keys, access, and the per-resident execute boolean
// only: no paths, no desktop apps,
// nothing else from local state. The house keeps it as a display snapshot for
// this epoch; the ceiling itself stays here and is what every request is
// checked against.
//
// On macOS it also carries one device-level boolean,
// execute_lifecycle_accepted: whether the person at the Mac has turned on
// "Let residents run commands on this Mac". It is display only, so the
// Computers page can say when that local opt-in is off. A missing field (Linux,
// or an older companion) means not reported, never false.
//
// Only residents enrolled on this device (the house's policy from the same
// "ready") are reported, so a local ceiling that also names someone else's
// resident never sends that ID or their volumes. Each reported resident keeps
// every locally allowed volume, including ones the house has off, because
// those are the choices the owner's page offers.
func ceilingReportFrame(identifier, epoch string, ceiling, enrolled policy.Policy, executeAccepted *bool) ([]byte, error) {
	if epoch == "" || ceiling.Validate() != nil || enrolled.Validate() != nil {
		return nil, errors.New("no valid ceiling to report")
	}
	onDevice := make(map[string]bool, len(enrolled.Residents))
	for _, r := range enrolled.Residents {
		onDevice[r.ID] = true
	}
	residents := make([]reportedResident, 0, len(ceiling.Residents))
	for _, r := range ceiling.Residents {
		if !onDevice[r.ID] {
			continue
		}
		volumes := make([]reportedVolume, 0, len(r.Volumes))
		for _, g := range r.Volumes {
			volumes = append(volumes, reportedVolume{Volume: g.Volume, Access: g.Access})
		}
		residents = append(residents, reportedResident{ID: r.ID, Execute: r.Execute, Volumes: volumes})
	}
	report := map[string]any{
		"action":           "report_ceiling",
		"connection_epoch": epoch,
		"ceiling":          map[string]any{"residents": residents},
	}
	if executeAccepted != nil {
		report["execute_lifecycle_accepted"] = *executeAccepted
	}
	data, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"command": "message", "identifier": identifier, "data": string(data)})
}
