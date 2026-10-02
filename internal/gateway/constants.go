package gateway

import (
	"time"
)

const (
	// DefaultPromptTTL bounds unanswered confirmations across all channels.
	DefaultPromptTTL            time.Duration = 5 * time.Minute
	promptIDBytes               int           = 16
	restReadTimeout             time.Duration = 30 * time.Second
	restWriteTimeout            time.Duration = 60 * time.Second
	restIdleTimeout             time.Duration = 2 * time.Minute
	restShutdownTimeout         time.Duration = 10 * time.Second
	restMessageMaxBytes         int64         = 1 << 20
	restPromptMaxBytes          int64         = 4096
	restTaskApprovalMaxBytes    int64         = 4096
	telegramUpdateMaxBytes      int64         = 1 << 20
	telegramAPIErrorMaxBytes    int64         = 1024
	telegramPollMaxBytes        int64         = 8 << 20
	telegramAPIResponseMaxBytes int64         = 1 << 20
	telegramAPIRequestTimeout   time.Duration = 30 * time.Second
	telegramDownloadTimeout     time.Duration = 30 * time.Second
	telegramFileMetadataTimeout time.Duration = 10 * time.Second
	telegramUnknownUserDedupTTL time.Duration = 5 * time.Minute
	slackAPIErrorMaxBytes       int64         = 1024
	slackAPIRequestTimeout      time.Duration = 30 * time.Second
	slackDownloadTimeout        time.Duration = 60 * time.Second
	slackConversationPageSize   int           = 200
	slackConversationMaxPages   int           = 10
	// Truncate takes a content width; reserve the ASCII ellipsis separately.
	toolDisplayEllipsis             = "..."
	toolArgumentDisplayWidth  int   = 60
	toolArgumentContentWidth        = toolArgumentDisplayWidth - len(toolDisplayEllipsis)
	toolArgumentsDisplayWidth int   = 80
	toolArgumentsContentWidth       = toolArgumentsDisplayWidth - len(toolDisplayEllipsis)
	webhookMaxRequestBytes    int64 = 64 << 10
	notifyResponseDrainBytes  int64 = 4 << 10
)
