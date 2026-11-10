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
	Volumes []reportedVolume `json:"volumes"`
}

// ceilingReportFrame builds the one Action Cable message that tells the house
// which volumes this Mac's local ceiling allows each resident. It carries
// volume keys and access only: no paths, no execute flag, nothing else from
// local state. The house keeps it as a display snapshot for this epoch; the
// ceiling itself stays here and is what every request is checked against.
func ceilingReportFrame(identifier, epoch string, ceiling policy.Policy) ([]byte, error) {
	if epoch == "" || ceiling.Validate() != nil {
		return nil, errors.New("no valid ceiling to report")
	}
	residents := make([]reportedResident, 0, len(ceiling.Residents))
	for _, r := range ceiling.Residents {
		volumes := make([]reportedVolume, 0, len(r.Volumes))
		for _, g := range r.Volumes {
			volumes = append(volumes, reportedVolume{Volume: g.Volume, Access: g.Access})
		}
		residents = append(residents, reportedResident{ID: r.ID, Volumes: volumes})
	}
	data, err := json.Marshal(map[string]any{
		"action":           "report_ceiling",
		"connection_epoch": epoch,
		"ceiling":          map[string]any{"residents": residents},
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"command": "message", "identifier": identifier, "data": string(data)})
}
