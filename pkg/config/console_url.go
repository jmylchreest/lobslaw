package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ValidateConsolePublicURL accepts a browser-reachable root without credentials.
func ValidateConsolePublicURL(value string) error {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || strings.TrimSpace(value) != value || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("%w: ui-web.public_url must be an absolute HTTP(S) root URL without credentials, query or fragment", types.ErrInvalidConfig)
	}
	return nil
}
