package commandrisk

import "strings"

// Only a plain simulation is a read. Configuration overrides can install
// hooks or turn simulation off, so unfamiliar options keep the usual labels.
func aptSimulation(args []riskToken) bool {
	simulated, verb := false, false
	for _, arg := range args {
		if arg.expands {
			return false
		}
		switch arg.text {
		case "-s", "--simulate", "--just-print", "--dry-run", "--recon", "--no-act":
			simulated = true
		case "-y", "--yes", "--assume-yes", "-q", "-qq", "--quiet":
		default:
			if strings.HasPrefix(arg.text, "-") {
				return false
			}
			if !verb {
				switch arg.text {
				case "install", "remove", "purge", "upgrade", "dist-upgrade", "autoremove":
					verb = true
				default:
					return false
				}
			}
		}
	}
	return simulated && verb
}
