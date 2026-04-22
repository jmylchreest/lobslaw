// Package config loads lobslaw configuration from TOML plus environment
// variable overrides, using github.com/knadh/koanf/v2 as the layered
// configuration library. Secret refs (env:FOO, file:/path, kms:arn) are
// resolved at load time.
package config
