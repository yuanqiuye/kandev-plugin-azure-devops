package hosttest

import (
	"context"
	"errors"
	"strconv"

	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SentMessage records one Host.Messages().Send invocation.
type SentMessage struct {
	TaskID    string
	SessionID string
	Text      string
}

// AddSession registers or updates a task session for Sessions().List and Messages().Send.
func (h *Host) AddSession(session pluginsdk.Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.sessions {
		if h.sessions[i].ID == session.ID {
			h.sessions[i] = session
			return
		}
	}
	h.sessions = append(h.sessions, session)
}

// SentMessages returns a copy of all messages sent through Messages().Send.
func (h *Host) SentMessages() []SentMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]SentMessage(nil), h.sentMessages...)
}

func (h *Host) Sessions() pluginsdk.SessionReader { return sessionReader{h} }
func (h *Host) Messages() pluginsdk.MessageReader { return messageReader{h} }

type sessionReader struct{ host *Host }

func (r sessionReader) List(_ context.Context, filter pluginsdk.SessionFilter, _ pluginsdk.Page) ([]pluginsdk.Session, *pluginsdk.PageInfo, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	var result []pluginsdk.Session
	for _, session := range r.host.sessions {
		if len(filter.TaskIDs) > 0 && !contains(filter.TaskIDs, session.TaskID) {
			continue
		}
		if len(filter.States) > 0 && !contains(filter.States, session.State) {
			continue
		}
		result = append(result, session)
	}
	return result, &pluginsdk.PageInfo{}, nil
}

func (r sessionReader) CodeStats(context.Context, pluginsdk.SessionFilter, pluginsdk.Page) ([]pluginsdk.SessionCodeStats, *pluginsdk.PageInfo, error) {
	return nil, &pluginsdk.PageInfo{}, nil
}

type messageReader struct{ host *Host }

func (r messageReader) List(context.Context, pluginsdk.MessageFilter, pluginsdk.Page) ([]pluginsdk.Message, *pluginsdk.PageInfo, error) {
	return nil, &pluginsdk.PageInfo{}, nil
}

func (r messageReader) Send(_ context.Context, taskID, sessionID, text string) (*pluginsdk.MessageDispatch, error) {
	r.host.mu.Lock()
	defer r.host.mu.Unlock()
	if r.host.FailSendMessage != nil {
		return nil, r.host.FailSendMessage
	}
	if taskID == "" || text == "" {
		return nil, errors.New("hosttest: taskID and text are required")
	}
	if _, found := r.host.tasks[taskID]; !found {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	resolvedSessionID := sessionID
	dispatchStatus := "sent"
	if resolvedSessionID == "" {
		for _, session := range r.host.sessions {
			if session.TaskID == taskID {
				resolvedSessionID = session.ID
				if session.State == "RUNNING" || session.State == "STARTING" {
					dispatchStatus = "queued"
				}
				break
			}
		}
		if resolvedSessionID == "" {
			resolvedSessionID = "session-auto-" + strconv.Itoa(len(r.host.sentMessages)+1)
			dispatchStatus = "started"
		}
	}
	r.host.sentMessages = append(r.host.sentMessages, SentMessage{
		TaskID:    taskID,
		SessionID: resolvedSessionID,
		Text:      text,
	})
	return &pluginsdk.MessageDispatch{SessionID: resolvedSessionID, Status: dispatchStatus}, nil
}
