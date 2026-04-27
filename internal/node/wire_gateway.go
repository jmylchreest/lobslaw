package node

import (
	"context"
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/mcp"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/singleton"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

func (n *Node) wireGateway() error {
	if n.agent == nil {
		return fmt.Errorf("gateway requires compute function (no agent wired on this node)")
	}

	n.promptRegistry = gateway.NewPromptRegistry()

	var tg *gateway.TelegramHandler
	var webhooks []*gateway.WebhookHandler
	for i, ch := range n.cfg.Gateway.Channels {
		switch ch.Type {
		case "telegram":
			h, err := n.buildTelegramHandler(ch)
			if err != nil {
				return fmt.Errorf("gateway.channels[%d] (telegram): %w", i, err)
			}
			tg = h
			n.telegramHandler = h
		case "webhook":
			h, err := n.buildWebhookHandler(ch)
			if err != nil {
				return fmt.Errorf("gateway.channels[%d] (webhook): %w", i, err)
			}
			webhooks = append(webhooks, h)
		case "rest":
			// REST is the base HTTP surface — no separate handler
			// to register; ignore so operators can list it explicitly.
		case "":
			n.log.Warn("gateway.channels[%d] has empty type; skipping", "index", i)
		default:
			n.log.Warn("gateway.channels: unknown type; skipping",
				"index", i, "type", ch.Type)
		}
	}
	n.webhookHandlers = webhooks

	// HTTPPort defaults to 8443 when unset. ListenAddr uses 0.0.0.0
	// unless the operator supplies a specific bind via future config
	// (Phase 6h keeps it simple; a bind-address setting lands with
	// notify_telegram builtin: now that telegram handler exists,
	// register the proactive-push builtin so commitments and
	// scheduled tasks can deliver out-of-band messages back to
	// chats. Skipped silently if Telegram isn't configured.
	if tg != nil && n.builtinsRegistry != nil && n.toolRegistry != nil {
		if err := compute.RegisterNotifyBuiltins(n.builtinsRegistry, compute.NotifyConfig{
			Telegram: tg,
		}); err != nil {
			n.log.Warn("notify: builtin register failed", "err", err)
		} else {
			for _, td := range compute.NotifyToolDefs() {
				if err := n.toolRegistry.Register(td); err != nil {
					n.log.Warn("notify: tool def register failed", "name", td.Name, "err", err)
				}
			}
			n.log.Debug("compute: notify_telegram registered")
		}
	}

	// HTTPPort=0 means "let the OS pick an ephemeral port" (test
	// + dev setup that doesn't care about a fixed bind). Shipped
	// configs in examples/ and deploy/docker/ both set http_port
	// explicitly, so this only affects callers that constructed
	// node.Config programmatically and left the field zero.
	port := n.cfg.Gateway.HTTPPort
	addr := fmt.Sprintf(":%d", port)

	// Pick a default TLS pair from the first channel that supplies
	// one — Telegram's webhook demands TLS, so if it's configured we
	// want its cert to front the REST surface too. No channel with
	// TLS → plaintext (fine for localhost + reverse-proxy-terminated
	// deployments; operators wanting public HTTPS supply a channel
	// with tls_cert/tls_key).
	var tlsCert, tlsKey string
	for _, ch := range n.cfg.Gateway.Channels {
		if ch.TLSCert != "" && ch.TLSKey != "" {
			tlsCert, tlsKey = ch.TLSCert, ch.TLSKey
			break
		}
	}

	cfg := gateway.RESTConfig{
		Addr:            addr,
		TLSCert:         tlsCert,
		TLSKey:          tlsKey,
		DefaultScope:    n.cfg.Gateway.UnknownUserScope,
		DefaultBudget:   compute.FromComputeConfig(n.cfg.Compute),
		JWTValidator:    n.jwtValidator,
		RequireAuth:     n.cfg.Auth.RequireAuth,
		Telegram:        tg,
		Webhooks:        webhooks,
		Prompts:         n.promptRegistry,
		ConfirmationTTL: n.cfg.Gateway.ConfirmationTimeout,
		Plan:            planServiceOrNil(n.planSvc),
		Logger:          n.log,
	}

	n.gatewaySrv = gateway.NewServer(cfg, n.agent)
	n.log.Info("gateway wired",
		"http_port", port,
		"tls", tlsCert != "",
		"channels", len(n.cfg.Gateway.Channels),
		"telegram", tg != nil,
		"require_auth", cfg.RequireAuth,
	)
	return nil
}

// buildTelegramHandler resolves bot token + webhook secret from the
// channel config's secret refs and constructs the handler. Secrets
// missing from the environment fail boot loudly — a Telegram channel
// with an empty token is a silent drop of every update.
func (n *Node) buildTelegramHandler(ch config.GatewayChannelConfig) (*gateway.TelegramHandler, error) {
	botToken, err := n.resolveChannelSecret(ch.BotTokenRef)
	if err != nil {
		return nil, fmt.Errorf("bot_token_ref %q: %w", ch.BotTokenRef, err)
	}
	if botToken == "" {
		return nil, fmt.Errorf("bot_token_ref %q resolved to empty — required for Telegram", ch.BotTokenRef)
	}

	mode := gateway.TelegramMode(ch.Mode)
	if mode == "" {
		mode = gateway.TelegramModeWebhook
	}

	var webhookSecret string
	if mode == gateway.TelegramModeWebhook {
		webhookSecret, err = n.resolveChannelSecret(ch.SecretTokenRef)
		if err != nil {
			return nil, fmt.Errorf("secret_token_ref %q: %w", ch.SecretTokenRef, err)
		}
		if webhookSecret == "" {
			return nil, fmt.Errorf("secret_token_ref %q resolved to empty — required for Telegram webhook (or set mode=\"poll\")", ch.SecretTokenRef)
		}
	}

	userScopes, err := parseUserScopes(ch.UserScopes)
	if err != nil {
		return nil, fmt.Errorf("user_scopes: %w", err)
	}

	// Leader-pinned long-poll: only the raft leader polls so multi-
	// node deployments don't fight over the bot token (Telegram only
	// delivers to one long-poller). Nil gate when raft isn't local
	// (gateway-only nodes) → today's behaviour, operator must ensure
	// only one such node runs the bot.
	var gate singleton.Gate
	if n.leaderGate != nil && mode == gateway.TelegramModePoll {
		gate = n.leaderGate
	}

	var channelState gateway.ChannelStateStore
	if n.raft != nil && n.store != nil {
		channelState = memory.NewChannelStateService(n.raft, n.store)
	}
	return gateway.NewTelegramHandler(gateway.TelegramConfig{
		BotToken:         botToken,
		Mode:             mode,
		WebhookSecret:    webhookSecret,
		UserIDScopes:     userScopes,
		UnknownUserScope: n.cfg.Gateway.UnknownUserScope,
		DefaultBudget:    compute.FromComputeConfig(n.cfg.Compute),
		Prompts:          n.promptRegistry,
		ConfirmationTTL:  n.cfg.Gateway.ConfirmationTimeout,
		TypingInterval:   n.cfg.Gateway.TypingInterval,
		InterimTimeout:   n.cfg.Gateway.InterimTimeout,
		HardTimeout:      n.cfg.Gateway.HardTimeout,
		Soul:             n.soulProvider,
		Logger:           n.log,
		Gate:             gate,
		ChannelState:     channelState,
	}, n.agent)
}

// soulProvider returns the current SOUL config if one is loaded,
// or nil when the node is running without a soul file. Passed to
// TelegramConfig so responsiveness timers can gate on SOUL
// characteristics without needing a direct dependency.
// buildWebhookHandler resolves the shared-secret ref and constructs
// a WebhookHandler. Fails on empty name or unresolvable secret;
// scope defaults to "webhook" at the handler layer.
func (n *Node) buildWebhookHandler(ch config.GatewayChannelConfig) (*gateway.WebhookHandler, error) {
	if ch.Name == "" {
		return nil, fmt.Errorf("webhook channel: name required (used in mount path and logs)")
	}
	secret, err := n.resolveChannelSecret(ch.SharedSecretRef)
	if err != nil {
		return nil, fmt.Errorf("webhook %q: shared_secret_ref: %w", ch.Name, err)
	}
	return gateway.NewWebhookHandler(gateway.WebhookConfig{
		Name:          ch.Name,
		Path:          ch.WebhookPath,
		SharedSecret:  secret,
		Scope:         ch.Scope,
		DefaultBudget: compute.FromComputeConfig(n.cfg.Compute),
		Logger:        n.log,
	}, n.agent)
}

// startMCPFromConfig spawns every [[mcp.servers]] entry, translating
// lobslaw's config schema into the mcp package's ServerConfig shape.
// Secret refs resolve via the channel resolver (same one Telegram
// uses). Plugin-provided MCP manifests still work independently.
// registerMCPToolsWithCompute adds each live MCP tool's ToolDef
// into the compute.Registry so the LLM sees them in its function
// list. Also chains the Loader into the agent's SkillDispatcher so
// tool calls with mcp-registered names dispatch through it.
// Called once after startMCPFromConfig; safe to call with zero
// tools (no-op).
func (n *Node) registerMCPToolsWithCompute() {
	if n.mcpLoader == nil || n.toolRegistry == nil {
		return
	}
	defs := n.mcpLoader.ToolDefs()
	for _, td := range defs {
		if err := n.toolRegistry.Register(td); err != nil {
			n.log.Warn("mcp: register tool def failed", "name", td.Name, "err", err)
		}
	}
	if n.agent != nil && len(defs) > 0 {
		n.agent.SetSkillDispatcher(compute.NewSkillDispatcherChain(
			skillDispatcherOrNil(n.skillAdapter),
			n.mcpLoader,
		))
	}
	if len(defs) > 0 {
		n.log.Info("mcp: registered tools with compute registry", "count", len(defs))
	}

	if n.builtinsRegistry != nil && n.toolRegistry != nil {
		if err := compute.RegisterMCPManagementBuiltins(n.builtinsRegistry, compute.MCPManagementConfig{
			Registry: n.mcpLoader,
		}); err != nil {
			n.log.Warn("mcp: register management builtins failed", "err", err)
		} else {
			for _, td := range compute.MCPManagementToolDefs() {
				if err := n.toolRegistry.Register(td); err != nil {
					n.log.Warn("mcp: register management tool def failed",
						"name", td.Name, "err", err)
				}
			}
			n.log.Debug("compute: mcp_list + mcp_add + mcp_remove registered")
		}
	}
}

func (n *Node) startMCPFromConfig(ctx context.Context) error {
	if n.mcpLoader == nil {
		n.mcpLoader = mcp.NewLoader(mcp.LoaderConfig{
			Logger:         n.log,
			SecretResolver: config.ResolveSecret,
			ProxyURL:       n.subprocessProxyURL,
		})
	}
	servers := make(map[string]mcp.ServerConfig, len(n.cfg.MCP.Servers))
	for name, s := range n.cfg.MCP.Servers {
		servers[name] = mcp.ServerConfig{
			Command:   s.Command,
			Args:      s.Args,
			Env:       s.Env,
			SecretEnv: s.SecretEnv,
			Disabled:  s.Disabled,
			Install:   s.Install,
		}
	}
	return n.mcpLoader.StartDirect(ctx, servers)
}
