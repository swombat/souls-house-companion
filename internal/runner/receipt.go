package runner

import "time"

// EscapeCandidate is an ambiguous detection receipt, never an automatic kill
// target. It is operation data: do not put this or process names in audit logs.
type EscapeCandidate struct {
	PID     int       `json:"pid"`
	PGID    int       `json:"pgid"`
	PPID    int       `json:"ppid"`
	Comm    string    `json:"comm"`
	Started time.Time `json:"started_at"`
}
