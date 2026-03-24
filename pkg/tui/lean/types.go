package lean

import (
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type SessionDescriptor struct {
	ID               string
	AgentName        string
	Model            string
	WorkingDirectory string
}

type UsageTotals struct {
	Prompt        int64
	Completion    int64
	Total         int64
	CacheRead     int64
	CacheCreation int64
	Cost          float64
}

type ToolStatus string

type ConnectionStatus string

const (
	ToolPending      ToolStatus = "pending"
	ToolConfirmation ToolStatus = "confirmation"
	ToolRunning      ToolStatus = "running"
	ToolCompleted    ToolStatus = "completed"
	ToolError        ToolStatus = "error"
)

const (
	ConnectionDisconnected ConnectionStatus = "disconnected"
	ConnectionConnected    ConnectionStatus = "connected"
)

type Options struct {
	FirstMessage           string
	QueuedMessages         []string
	FirstMessageAttachment string
	ExitAfterFirstResponse bool
}

func DescriptorFromState(rt runtime.Runtime, sess *session.Session) SessionDescriptor {
	if sess == nil {
		return SessionDescriptor{AgentName: rt.CurrentAgentName()}
	}
	return SessionDescriptor{
		ID:               sess.ID,
		AgentName:        rt.CurrentAgentName(),
		WorkingDirectory: sess.WorkingDir,
	}
}
