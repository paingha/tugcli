package plugin

import (
	"debug/elf"
	"debug/macho"
	"debug/pe"
)

// binaryPlatform reports the OS and architecture an executable was built for.
// It returns empty strings for scripts and formats it does not recognise,
// which are treated as platform-neutral.
func binaryPlatform(path string) (goos, goarch string) {
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		switch f.Machine {
		case elf.EM_X86_64:
			return "linux", "amd64"
		case elf.EM_AARCH64:
			return "linux", "arm64"
		}
		return "linux", ""
	}
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		return "darwin", machoArch(f.Cpu)
	}
	if f, err := macho.OpenFat(path); err == nil {
		defer f.Close()
		return "darwin", "" // universal binaries run on every Mac
	}
	if f, err := pe.Open(path); err == nil {
		defer f.Close()
		switch f.Machine {
		case pe.IMAGE_FILE_MACHINE_AMD64:
			return "windows", "amd64"
		case pe.IMAGE_FILE_MACHINE_ARM64:
			return "windows", "arm64"
		}
		return "windows", ""
	}
	return "", ""
}

func machoArch(cpu macho.Cpu) string {
	switch cpu {
	case macho.CpuAmd64:
		return "amd64"
	case macho.CpuArm64:
		return "arm64"
	default:
		return ""
	}
}

// matchesPlatform reports whether a binary can run on p. Unknown formats
// (scripts) match everything.
func matchesPlatform(path string, p Platform) bool {
	goos, goarch := binaryPlatform(path)
	return (goos == "" || goos == p.OS) && (goarch == "" || goarch == p.Arch)
}
