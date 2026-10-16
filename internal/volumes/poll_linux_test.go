package volumes

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestPollRetriesInterruptionsNotEvents(t *testing.T) {
	calls := 0
	if pollChanged(7, func(p []unix.PollFd, timeout int) (int, error) {
		calls++
		if p[0].Fd != 7 || timeout != 0 {
			t.Fatal("wrong poll")
		}
		if calls < 4 {
			return 0, unix.EINTR
		}
		return 0, nil
	}) || calls != 4 {
		t.Fatal("interruption treated as mount event")
	}
	if !pollChanged(7, func(p []unix.PollFd, _ int) (int, error) {
		p[0].Revents = unix.POLLPRI
		return 1, unix.EINTR
	}) {
		t.Fatal("actual event lost")
	}
	if !pollChanged(7, func([]unix.PollFd, int) (int, error) { return 0, unix.EINTR }) {
		t.Fatal("unbounded interruption accepted")
	}
}
