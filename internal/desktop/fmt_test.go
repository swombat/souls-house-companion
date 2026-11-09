package desktop

import "fmt"

func fmtSscan(s string, v any) (int, error) { return fmt.Sscan(s, v) }
