package handler

import (
	"github.com/m1k1o/neko/server/pkg/types"
	"github.com/m1k1o/neko/server/pkg/types/event"
	"github.com/m1k1o/neko/server/pkg/types/message"
)

func (h *MessageHandlerCtx) screenShareStart(session types.Session) error {
	err := h.webrtc.StartScreenShare(session)
	if err != nil {
		return err
	}

	// send status to the session that started sharing
	session.Send(
		event.SCREEN_SHARE_STATUS,
		message.ScreenShareStatus{
			IsActive:         true,
			SharingSessionID: session.ID(),
		})

	return nil
}

func (h *MessageHandlerCtx) screenShareStop(session types.Session) error {
	err := h.webrtc.StopScreenShare(session)
	if err != nil {
		return err
	}

	return nil
}
