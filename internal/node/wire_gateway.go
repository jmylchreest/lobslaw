package node

import (
	"context"
	"fmt"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/mcp"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/notify"
	"github.com/jmylchreest/lobslaw/internal/singleton"
	"github.com/jmylchreest/lobslaw/internal/tools"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

func (n *Node) wireGateway() error {
	if n.agent == nil {
		return fmt.Errorf("gateway requires compute function (no agent wired on this node)")
	}

	if err := n.wirePrompts(); err != nil {
		return err
	}

	var tg *gateway.TelegramHandler
	var sl *gateway.SlackHandler
	var webhooks []*gateway.WebhookHandler
	for i, ch := range n.cfg.Gateway.Channels {
		switch ch.Type {
		case "slack":
			h, err := n.buildSlackHandler(ch)
			if err != nil {
				return fmt.Errorf("gateway.channels[%d] (slack): %w", i, err)
			}
			sl = h
		case "telegram":
			h, err := n.buildTelegramHandler(ch)
			if err != nil {
				return fmt.Errorf("gateway.channels[%d] (telegram): %w", i, err)
			}
			tg = h
			n.telegramHandler = h
			// Both directions of the enrolment loop. The handler was
			// built with Enrolments already set; this is the half that
			// could not be, because the handler did not exist yet.
			n.attachEnrolmentAsker(h)
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

	n.registerSlackTools(sl)
	n.wireNotifySinks(tg, sl)

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
		Notices:          n.notices,
		QueueMode:        gateway.ParseQueueMode(n.cfg.Gateway.QueueMode),
		QueueDebounce:    n.cfg.Gateway.QueueDebounce,
		RelatednessJudge: n.newRelatednessJudge(),
		QueueBurstWindow: n.cfg.Gateway.QueueBurstWindow,
		QueueBurstReset:  n.cfg.Gateway.QueueBurstReset,
		Leaser:           n.newSessionLeaser(),
		Addr:             addr,
		TypingInterval:   n.cfg.Gateway.TypingInterval,
		InterimTimeout:   n.cfg.Gateway.InterimTimeout,
		HardTimeout:      n.cfg.Gateway.HardTimeout,
		ReadTimeout:      n.cfg.Gateway.ReadTimeout,
		WriteTimeout:     restWriteTimeout(n.cfg.Gateway),
		IdleTimeout:      n.cfg.Gateway.IdleTimeout,
		Soul:             n.soulProvider,
		TLSCert:          tlsCert,
		TLSKey:           tlsKey,
		DefaultScope:     n.cfg.Gateway.UnknownUserScope,
		DefaultBudget:    compute.FromComputeConfig(n.cfg.Compute),
		JWTValidator:     n.jwtValidator,
		RequireAuth:      n.cfg.Auth.RequireAuth,
		Telegram:         tg,
		Slack:            sl,
		Webhooks:         webhooks,
		Prompts:          n.promptRegistry,
		ConfirmationTTL:  n.cfg.Gateway.ConfirmationTimeout,
		Plan:             planServiceOrNil(n.planSvc),
		Sessions:         n.newSessionStore(),
		Compactor:        n.newSessionCompactor(),
		Conversation:     n.conversationConfig(),
		Logger:           n.log,
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

// registerSlackTools exposes slack_read_channel / slack_search.
//
// Registered here rather than alongside the other builtins in
// wireCompute, because they need the handler and the gateway is wired
// second. seedDefaultPolicyRules runs later still, in Start, so it sees
// them — and skips them, since they are in noSeedTools.
func (n *Node) registerSlackTools(sl *gateway.SlackHandler) {
	if sl == nil || n.builtinsRegistry == nil || n.toolRegistry == nil {
		return
	}
	if err := tools.RegisterSlackBuiltins(n.builtinsRegistry, tools.SlackToolConfig{
		Reader: sl,
	}); err != nil {
		n.log.Warn("slack: read tools not registered", "err", err)
		return
	}
	for _, td := range tools.SlackToolDefs() {
		if err := n.toolRegistry.Register(td); err != nil {
			n.log.Warn("slack: tool def register failed", "name", td.Name, "err", err)
		}
	}
	n.log.Debug("compute: slack read tools registered")
}

// wireNotifySinks routes the channel-agnostic `notify` builtin through
// each channel that can deliver outbound. Per-user channel addresses
// live in BucketUserPrefs, seeded from [[user]] config at boot.
func (n *Node) wireNotifySinks(tg *gateway.TelegramHandler, sl *gateway.SlackHandler) {
	if n.builtinsRegistry == nil || n.toolRegistry == nil || n.userPrefsSvc == nil {
		return
	}
	notifySvc := notify.NewService(n.userPrefsSvc, n.log)
	if tg != nil {
		if err := notifySvc.RegisterSink(&gateway.TelegramSink{Handler: tg}); err != nil {
			n.log.Warn("notify: telegram sink register failed", "err", err)
		}
	}
	if sl != nil {
		if err := notifySvc.RegisterSink(&gateway.SlackSink{Handler: sl}); err != nil {
			n.log.Warn("notify: slack sink register failed", "err", err)
		}
	}
	if err := notifySvc.RegisterSink(&gateway.RESTSink{}); err != nil {
		n.log.Warn("notify: rest sink register failed", "err", err)
	}
	// The callback sink is what makes asynchronous work reachable over
	// REST at all — see gateway.CallbackSink. Registered
	// unconditionally: it costs nothing until somebody binds a callback
	// address, and registering it only when one already exists would
	// leave a node that gains a user later with no sink for them until
	// it restarted.
	if err := notifySvc.RegisterSink(&gateway.CallbackSink{
		Client: egress.For("gateway/callback").HTTPClient(),
		Logger: n.log,
	}); err != nil {
		n.log.Warn("notify: callback sink register failed", "err", err)
	}
	n.notifySvc = notifySvc

	if err := tools.RegisterNotifyBuiltins(n.builtinsRegistry, tools.NotifyConfig{
		Service: notifySvc,
	}); err != nil {
		n.log.Warn("notify: builtin register failed", "err", err)
		return
	}
	for _, td := range tools.NotifyToolDefs() {
		if err := n.toolRegistry.Register(td); err != nil {
			n.log.Warn("notify: tool def register failed", "name", td.Name, "err", err)
		}
	}
	n.log.Debug("compute: notify registered")
}

// buildSlackHandler resolves both Slack tokens and constructs the
// handler. Two tokens with different jobs: the bot token signs Web API
// calls, the app token opens Socket Mode. Either missing is fatal at
// boot rather than at first message — a Slack channel that cannot
// connect or cannot reply is not a degraded channel, it is a silent one.
func (n *Node) buildSlackHandler(ch config.GatewayChannelConfig) (*gateway.SlackHandler, error) {
	botToken, err := n.resolveChannelSecret(ch.BotTokenRef)
	if err != nil {
		return nil, fmt.Errorf("bot_token_ref %q: %w", ch.BotTokenRef, err)
	}
	if botToken == "" {
		return nil, fmt.Errorf("bot_token_ref %q resolved to empty — required for Slack (the xoxb- token)", ch.BotTokenRef)
	}
	appToken, err := n.resolveChannelSecret(ch.AppTokenRef)
	if err != nil {
		return nil, fmt.Errorf("app_token_ref %q: %w", ch.AppTokenRef, err)
	}
	if appToken == "" {
		return nil, fmt.Errorf("app_token_ref %q resolved to empty — required for Slack Socket Mode (the xapp- token, needs connections:write)", ch.AppTokenRef)
	}

	// Slack delivers each event to exactly ONE open Socket Mode
	// connection. Two nodes both connected would split a conversation
	// between them at random, so the loop is pinned to the leader
	// wherever there is one to pin it to.
	var gate singleton.Gate
	if n.leaderGate != nil {
		gate = n.leaderGate
	}

	return gateway.NewSlackHandler(gateway.SlackConfig{
		BotToken:          botToken,
		AppToken:          appToken,
		AllowedChannels:   ch.AllowedChannels,
		UserScopes:        ch.UserScopes,
		UnknownUserScope:  n.cfg.Gateway.UnknownUserScope,
		Roles:             n.resolveUserRoles,
		Identity:          n.identityResolver(),
		DefaultBudget:     compute.FromComputeConfig(n.cfg.Compute),
		Notices:           n.notices,
		Prompts:           n.promptRegistry,
		ConfirmationTTL:   n.cfg.Gateway.ConfirmationTimeout,
		Approvals:         n.approvals,
		ApprovalRules:     n.approvalRules,
		Leaser:            n.newSessionLeaser(),
		Sessions:          n.newSessionStore(),
		Compactor:         n.newSessionCompactor(),
		Conversation:      n.conversationConfig(),
		QueueMode:         gateway.ParseQueueMode(n.cfg.Gateway.QueueMode),
		QueueDebounce:     n.cfg.Gateway.QueueDebounce,
		RelatednessJudge:  n.newRelatednessJudge(),
		QueueBurstWindow:  n.cfg.Gateway.QueueBurstWindow,
		QueueBurstReset:   n.cfg.Gateway.QueueBurstReset,
		TypingInterval:    n.cfg.Gateway.TypingInterval,
		InterimTimeout:    n.cfg.Gateway.InterimTimeout,
		HardTimeout:       n.cfg.Gateway.HardTimeout,
		Soul:              n.soulProvider,
		ArtifactOpener:    n.artifactOpener(),
		IncomingDir:       n.incomingDir(),
		CommandAuthorizer: n.commandAuthorizerOrNil(),
		SessionGrants:     n.sessionGrantsView(),
		Gate:              gate,
		Logger:            n.log,
	}, n.agent)
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
		IncomingDir:      n.incomingDir(),
		Notices:          n.notices,
		QueueMode:        gateway.ParseQueueMode(n.cfg.Gateway.QueueMode),
		QueueDebounce:    n.cfg.Gateway.QueueDebounce,
		RelatednessJudge: n.newRelatednessJudge(),
		QueueBurstWindow: n.cfg.Gateway.QueueBurstWindow,
		QueueBurstReset:  n.cfg.Gateway.QueueBurstReset,
		Approvals:        n.approvals,
		ApprovalRules:    n.approvalRules,
		Identity:         n.identityResolver(),
		Leaser:           n.newSessionLeaser(),
		ArtifactOpener:   n.artifactOpener(),
		BotToken:         botToken,
		Mode:             mode,
		WebhookSecret:    webhookSecret,
		UserIDScopes:     userScopes,
		// Nil when enrolment is not wired, which disables channel
		// approval and leaves the CLI path working.
		Enrolments:        n.enrolmentDecider(),
		CommandAuthorizer: n.commandAuthorizerOrNil(),
		SessionGrants:     n.sessionGrantsView(),
		Roles:             n.resolveUserRoles,
		UnknownUserScope:  n.cfg.Gateway.UnknownUserScope,
		DefaultBudget:     compute.FromComputeConfig(n.cfg.Compute),
		Prompts:           n.promptRegistry,
		ConfirmationTTL:   n.cfg.Gateway.ConfirmationTimeout,
		TypingInterval:    n.cfg.Gateway.TypingInterval,
		InterimTimeout:    n.cfg.Gateway.InterimTimeout,
		HardTimeout:       n.cfg.Gateway.HardTimeout,
		Soul:              n.soulProvider,
		Logger:            n.log,
		Gate:              gate,
		ChannelState:      channelState,
		Sessions:          n.newSessionStore(),
		Compactor:         n.newSessionCompactor(),
		Conversation:      n.conversationConfig(),
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
// into the tools.Registry so the LLM sees them in its function
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
		// MCP tool defs come from external MCP servers — go through
		// RegisterExternal so a hostile/buggy server can't ship a
		// tool with a builtin: path that would short-circuit our
		// privileged in-process dispatcher.
		if err := n.toolRegistry.RegisterExternal(td); err != nil {
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
		if err := tools.RegisterMCPManagementBuiltins(n.builtinsRegistry, tools.MCPManagementConfig{
			Registry: n.mcpLoader,
		}); err != nil {
			n.log.Warn("mcp: register management builtins failed", "err", err)
		} else {
			for _, td := range tools.MCPManagementToolDefs() {
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
			Logger: n.log,
			// Through the node's resolver, not pkg/config directly:
			// an MCP server's secret_env is as entitled to name a
			// vault as a provider key is, and this was the one path
			// that still bypassed the injected resolver.
			SecretResolver: n.resolveChannelSecret,
			// MCP servers don't carry the network_isolation flag —
			// the manifest field is skill-only. Always pass false.
			ProxyURL: func(role string) string { return n.subprocessProxyURL(role, false) },
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

// newRelatednessJudge builds the classifier queue_mode = "smart"
// consults, on the PREFLIGHT role.
//
// The same role the routing judge uses, and for the same reason: a
// small fast model doing classification ahead of the main turn. Nil
// when no role map exists, which the gate reads as "no judge" and
// falls back to folding — so smart on a node without compute is
// debounce rather than an error.
//
// Deliberately NOT gated on [[compute.chains]]. The routing judge is,
// because a route with no chains to pick between decides nothing;
// this one decides something on every message regardless.
func (n *Node) newRelatednessJudge() gateway.RelatednessJudge {
	if n.roleMap == nil {
		return nil
	}
	label := n.cfg.Compute.Roles.Preflight
	if label == "" {
		label = n.primaryProviderLabel()
	}
	var model string
	for _, p := range n.cfg.Compute.Providers {
		if p.Label == label {
			model = p.Model
			break
		}
	}
	j := compute.NewRelatednessJudge(n.roleMap.For(compute.RolePreflight), model, n.log)
	if j == nil {
		return nil
	}
	return j
}

// restWriteTimeout derives the REST server's write deadline from the
// turn's own cap.
//
// WriteTimeout bounds the entire request-to-response window, so one
// shorter than HardTimeout can only ever truncate a turn the agent was
// still entitled to finish: the socket dies first and the caller gets
// "Empty reply from server" for a turn that completed server-side and
// wrote its artifacts. The two deadlines have to be ordered outward.
//
// An explicit write_timeout is honoured as given — an operator who
// names a number means it. Otherwise this tracks HardTimeout so
// raising one does not silently require raising the other.
func restWriteTimeout(g config.GatewayConfig) time.Duration {
	if g.WriteTimeout > 0 {
		return g.WriteTimeout
	}
	hard := g.HardTimeout
	if hard <= 0 {
		hard = gateway.DefaultHardTimeout
	}
	// Margin so the agent's own forced-summary path is what ends a slow
	// turn, not the socket.
	return hard + 30*time.Second
}
