package clawhub

import (
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/binaries"
)

// SynthesizeInstallSpecs translates a clawdbot.install array into the
// internal/binaries InstallSpec form Satisfier.Satisfy expects.
// One clawdbot.install entry produces one InstallSpec; entries with
// unrecognised kinds are dropped (with a wrapped error returned so
// the caller can decide whether to fail or proceed).
func SynthesizeInstallSpecs(installs []ClawdbotInstall) ([]binaries.InstallSpec, []string, error) {
	var specs []binaries.InstallSpec
	var skipped []string
	for _, in := range installs {
		mgr := ManagerKind(in.Kind)
		if mgr == "" {
			skipped = append(skipped, fmt.Sprintf("kind=%q (no matching manager)", in.Kind))
			continue
		}
		spec := binaries.InstallSpec{
			OS:      in.OS,
			Arch:    in.Arch,
			Distro:  in.Distro,
			Manager: mgr,
			Sudo:    in.Sudo,
			Args:    in.Args,
		}
		switch mgr {
		case "brew":
			spec.Package = in.Formula
			if spec.Package == "" {
				spec.Package = in.Package
			}
		case "curl-sh":
			spec.URL = in.URL
			spec.Checksum = in.Checksum
		default:
			spec.Package = in.Package
			if spec.Package == "" {
				// Some bundles use `formula` even for non-brew kinds;
				// accept that as a fallback.
				spec.Package = in.Formula
			}
		}
		// Default OS for managers where it's implied: brew runs on
		// linux+darwin, the rest are OS-implied by the manager
		// availability (apt/dnf/pacman/apk are linux-only). When
		// the bundle author omits OS we default to linux for system
		// managers; brew → empty (matches both linux and darwin).
		if spec.OS == "" {
			switch mgr {
			case "apt", "dnf", "pacman", "apk":
				spec.OS = "linux"
			case "brew":
				// Leave OS empty; brewManager.Available will reject on
				// hosts without brew anyway.
			}
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 && len(skipped) > 0 {
		return nil, skipped, fmt.Errorf("clawhub: no usable install specs in clawdbot.install (skipped: %v)", skipped)
	}
	return specs, skipped, nil
}
