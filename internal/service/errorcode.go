package service

import (
	"errors"
	"io/fs"

	"github.com/swombat/souls-house-companion/internal/linuxfs"
	"github.com/swombat/souls-house-companion/internal/policy"
)

// ErrorCode names a refusal with one fixed word for the house receipt. It
// never carries the error text, a path or an OS message. Protection is tested
// first so that a protected target is always reported as "protected", never as
// missing or as a symlink. linuxfs also refuses a protected path by name before
// it opens anything, so nothing inside a protected subtree can produce another
// code. Anything not listed here is reported as the generic "failed".
func (h *Handler) ErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, linuxfs.ErrProtected):
		return "protected"
	case errors.Is(err, ErrDenied), errors.Is(err, policy.ErrDenied):
		return "denied"
	case errors.Is(err, linuxfs.ErrSymlink):
		return "symlink"
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	}
	return ""
}
