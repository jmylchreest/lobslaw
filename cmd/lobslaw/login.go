package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

const loginUsage = `lobslaw login — print a one-time code for the web console

  lobslaw login --config <path> [--user <id>]

Talks to the running node on loopback (the console's HTTP port) and
prints a code. Type that code in the browser. It works once and expires
in a few minutes.

This has to run on the same machine as the node. There is no password
and no self-signup: the user must already be in [[user]].
`

func dispatchLogin(args []string) bool {
	idx := findSubcmd(args, "login")
	if idx < 0 {
		return false
	}
	lobslawLogin(args[idx+1:])
	return true
}

func lobslawLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("LOBSLAW_CONFIG", ""), "path to config.toml")
	user := fs.String("user", "", "[[user]] id; default is the only enrolled user")
	_ = fs.Parse(args)
	if *cfgPath == "" {
		exitWith("login: --config (or LOBSLAW_CONFIG) required")
	}
	cfg, err := config.Load(config.LoadOptions{Path: *cfgPath})
	if err != nil {
		exitWith(fmt.Sprintf("login: load config: %v", err))
	}
	port := cfg.Gateway.HTTPPort
	if port == 0 {
		port = config.DefaultGatewayHTTPPort
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/session/code", port)
	body := "{}"
	if strings.TrimSpace(*user) != "" {
		b, _ := json.Marshal(map[string]string{"user": strings.TrimSpace(*user)})
		body = string(b)
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		exitWith(fmt.Sprintf("login: %v", err))
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		exitWith(fmt.Sprintf("login: cannot reach the node at %s — is it running?\n%v", url, err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		var parsed struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &parsed) == nil && parsed.Error != "" {
			msg = parsed.Error
		}
		exitWith(fmt.Sprintf("login: %s", msg))
	}
	var out struct {
		Code      string `json:"code"`
		UserID    string `json:"user_id"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Code == "" {
		exitWith("login: node replied without a code")
	}
	pretty := out.Code
	if len(pretty) == 6 {
		pretty = pretty[:3] + " " + pretty[3:]
	}
	fmt.Fprintf(os.Stdout, "Sign-in code for %s (expires in %ds):\n\n  %s\n\nEnter it in the console. It works once.\n",
		out.UserID, out.ExpiresIn, pretty)
}
