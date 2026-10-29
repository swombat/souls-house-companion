//go:build darwin

package volumes

import (
	"os"
	"testing"
)

// TestDarwinAcceptanceSystemDataEvidence reports topology evidence, not approval.
// It reads kernel metadata and opens only the two public System/Data mount roots.
// No UUIDs, device numbers, FSIDs, source devices or personal paths are logged.
func TestDarwinAcceptanceSystemDataEvidence(t *testing.T) {
	if os.Getenv("COMPANION_DARWIN_ACCEPTANCE") != "1" {
		t.Skip("requires explicit real-Mac mount-discovery acceptance opt-in")
	}
	snapshot, err := SnapshotCurrent()
	if err != nil {
		t.Fatal("mount snapshot unavailable")
	}
	defer snapshot.Close()
	type evidence struct {
		row darwinRow
		fd  darwinEvidence
		ok  bool
	}
	selected := map[string]evidence{}
	for _, row := range snapshot.rows {
		if row.Point != "/" && row.Point != "/System/Volumes/Data" {
			continue
		}
		fd, probeErr := probeDarwinRoot(row)
		selected[row.Point] = evidence{row, fd, probeErr == nil && fd.uuid != ""}
	}
	root, data := selected["/"], selected["/System/Volumes/Data"]
	t.Logf("system_evidence_available=%t data_evidence_available=%t", root.ok, data.ok)
	if root.ok && data.ok {
		t.Logf("equal_st_dev=%t equal_fsid=%t equal_uuid=%t",
			root.fd.dev == data.fd.dev, root.row.FSID == data.row.FSID,
			root.fd.uuid == data.fd.uuid)
	}
	// Preserve the full mount table's ambiguity checks, but supply only the
	// two roots' evidence. Other mounts are deliberately not probed: these
	// scoped discovery receipts are NOT full-system volume approval.
	mounts := discoverDarwin(snapshot.rows, func(row darwinRow) (darwinEvidence, error) {
		e, found := selected[row.Point]
		if !found || !e.ok || e.row != row {
			return darwinEvidence{}, ErrMountChanged
		}
		return e.fd, nil
	})
	for _, m := range mounts {
		switch m.Mountpoint {
		case "/":
			t.Logf("scoped_system_denial=%q", m.Denied)
		case "/System/Volumes/Data":
			t.Logf("scoped_data_denial=%q", m.Denied)
		}
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal("mount snapshot changed during evidence collection")
	}
	// Passing means evidence was collected, not that either volume is approved.
	// TestDarwinAcceptanceTempAndDiscovery owns actual full-discovery acceptance.
}
