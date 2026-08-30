package compute

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
