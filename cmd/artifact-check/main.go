// artifact-check inspects cross-built artifacts on the build host.
package main

import (
	"debug/macho"
	"fmt"
	"os"
)

func check(path string, cpu macho.Cpu) error {
	f, err := macho.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if f.Cpu != cpu || f.Type != macho.TypeExec {
		return fmt.Errorf("unexpected Mach-O target or type")
	}
	if cpu == macho.CpuArm64 {
		signed := false
		for _, load := range f.Loads {
			data := load.Raw()
			if len(data) >= 16 && f.ByteOrder.Uint32(data[:4]) == 0x1d {
				off, size := f.ByteOrder.Uint32(data[8:12]), f.ByteOrder.Uint32(data[12:16])
				st, e := os.Stat(path)
				signed = e == nil && size > 0 && uint64(off)+uint64(size) <= uint64(st.Size())
			}
		}
		if !signed {
			return fmt.Errorf("arm64 LC_CODE_SIGNATURE absent or outside artifact")
		}
	}
	return nil
}
func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: artifact-check arm64-binary amd64-binary")
		os.Exit(2)
	}
	if check(os.Args[1], macho.CpuArm64) != nil || check(os.Args[2], macho.CpuAmd64) != nil {
		fmt.Fprintln(os.Stderr, "Mach-O artifact verification failed")
		os.Exit(1)
	}
	fmt.Println("Darwin targets verified; arm64 LC_CODE_SIGNATURE present (not notarisation/runtime proof)")
}
