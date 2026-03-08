package webrtc

import (
	"errors"
	"io"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/m1k1o/neko/server/pkg/types"
	"github.com/m1k1o/neko/server/pkg/types/event"
	"github.com/m1k1o/neko/server/pkg/types/message"
)

var (
	ErrScreenShareAlreadyActive = errors.New("screen share is already active")
	ErrScreenShareNotActive     = errors.New("screen share is not active")
	ErrScreenShareNotAllowed    = errors.New("not allowed to share media")
)

// ScreenShareManager manages screen share relay between peers.
// It receives RTP packets from the sharing peer and forwards them to all other peers.
type ScreenShareManager struct {
	mu     sync.RWMutex
	logger zerolog.Logger

	active           bool
	sharingSessionID string

	// the source track from the sharer (video)
	sourceVideoTrack *webrtc.TrackRemote
	// the source track from the sharer (audio)
	sourceAudioTrack *webrtc.TrackRemote

	// local tracks that relay to other peers
	relayVideoTrack *webrtc.TrackLocalStaticRTP
	relayAudioTrack *webrtc.TrackLocalStaticRTP

	// sessions manager for broadcasting events
	sessions types.SessionManager

	// stop channel for the relay goroutines
	videoStopCh chan struct{}
	audioStopCh chan struct{}
}

func NewScreenShareManager(sessions types.SessionManager) *ScreenShareManager {
	return &ScreenShareManager{
		logger:   log.With().Str("module", "webrtc").Str("submodule", "screenshare").Logger(),
		sessions: sessions,
	}
}

// IsActive returns whether a screen share is currently active.
func (s *ScreenShareManager) IsActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

// SharingSessionID returns the session ID of the user currently sharing.
func (s *ScreenShareManager) SharingSessionID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sharingSessionID
}

// Status returns the current screen share status message.
func (s *ScreenShareManager) Status() message.ScreenShareStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return message.ScreenShareStatus{
		IsActive:         s.active,
		SharingSessionID: s.sharingSessionID,
	}
}

// Start begins a screen share session for the given session.
func (s *ScreenShareManager) Start(session types.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !session.Profile().CanShareMedia {
		return ErrScreenShareNotAllowed
	}

	if s.active {
		return ErrScreenShareAlreadyActive
	}

	s.active = true
	s.sharingSessionID = session.ID()

	s.logger.Info().
		Str("session_id", session.ID()).
		Msg("screen share started")

	// broadcast status to all sessions
	s.sessions.Broadcast(
		event.SCREEN_SHARE_STATUS,
		message.ScreenShareStatus{
			IsActive:         true,
			SharingSessionID: session.ID(),
		})

	return nil
}

// Stop ends the current screen share session.
func (s *ScreenShareManager) Stop(session types.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.active {
		return ErrScreenShareNotActive
	}

	// only the sharing session or an admin can stop the share
	if s.sharingSessionID != session.ID() && !session.Profile().IsAdmin {
		return errors.New("not allowed to stop another user's screen share")
	}

	s.stopRelay()

	s.logger.Info().
		Str("session_id", session.ID()).
		Msg("screen share stopped")

	// broadcast status to all sessions
	s.sessions.Broadcast(
		event.SCREEN_SHARE_STATUS,
		message.ScreenShareStatus{
			IsActive:         false,
			SharingSessionID: "",
		})

	return nil
}

// ForceStop stops the screen share without permission checks (used on disconnect).
func (s *ScreenShareManager) ForceStop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.active {
		return
	}

	s.stopRelay()

	s.logger.Info().Msg("screen share force stopped")

	s.sessions.Broadcast(
		event.SCREEN_SHARE_STATUS,
		message.ScreenShareStatus{
			IsActive:         false,
			SharingSessionID: "",
		})
}

// stopRelay cleans up relay state (must be called with lock held).
func (s *ScreenShareManager) stopRelay() {
	// stop video relay
	if s.videoStopCh != nil {
		close(s.videoStopCh)
		s.videoStopCh = nil
	}

	// stop audio relay
	if s.audioStopCh != nil {
		close(s.audioStopCh)
		s.audioStopCh = nil
	}

	s.active = false
	s.sharingSessionID = ""
	s.sourceVideoTrack = nil
	s.sourceAudioTrack = nil
	s.relayVideoTrack = nil
	s.relayAudioTrack = nil
}

// SetVideoTrack sets the incoming video track from the screen sharer and creates the relay track.
func (s *ScreenShareManager) SetVideoTrack(track *webrtc.TrackRemote) (*webrtc.TrackLocalStaticRTP, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.active {
		return nil, ErrScreenShareNotActive
	}

	s.sourceVideoTrack = track

	// create a local track to relay video to other peers
	relayTrack, err := webrtc.NewTrackLocalStaticRTP(
		track.Codec().RTPCodecCapability,
		"screen-share-video",
		"screen-share",
	)
	if err != nil {
		return nil, err
	}

	s.relayVideoTrack = relayTrack
	s.videoStopCh = make(chan struct{})

	// start forwarding RTP packets from remote track to local relay track
	go s.relayRTP(track, relayTrack, s.videoStopCh, "video")

	return relayTrack, nil
}

// SetAudioTrack sets the incoming audio track from the screen sharer and creates the relay track.
func (s *ScreenShareManager) SetAudioTrack(track *webrtc.TrackRemote) (*webrtc.TrackLocalStaticRTP, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.active {
		return nil, ErrScreenShareNotActive
	}

	s.sourceAudioTrack = track

	// create a local track to relay audio to other peers
	relayTrack, err := webrtc.NewTrackLocalStaticRTP(
		track.Codec().RTPCodecCapability,
		"screen-share-audio",
		"screen-share",
	)
	if err != nil {
		return nil, err
	}

	s.relayAudioTrack = relayTrack
	s.audioStopCh = make(chan struct{})

	// start forwarding RTP packets from remote track to local relay track
	go s.relayRTP(track, relayTrack, s.audioStopCh, "audio")

	return relayTrack, nil
}

// GetRelayVideoTrack returns the current relay video track (for adding to new peer connections).
func (s *ScreenShareManager) GetRelayVideoTrack() *webrtc.TrackLocalStaticRTP {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.relayVideoTrack
}

// GetRelayAudioTrack returns the current relay audio track (for adding to new peer connections).
func (s *ScreenShareManager) GetRelayAudioTrack() *webrtc.TrackLocalStaticRTP {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.relayAudioTrack
}

// relayRTP forwards RTP packets from a remote track to a local relay track.
func (s *ScreenShareManager) relayRTP(remote *webrtc.TrackRemote, local *webrtc.TrackLocalStaticRTP, stopCh chan struct{}, kind string) {
	s.logger.Info().Str("kind", kind).Msg("starting screen share RTP relay")

	buf := make([]byte, 1500)
	for {
		select {
		case <-stopCh:
			s.logger.Info().Str("kind", kind).Msg("screen share RTP relay stopped")
			return
		default:
			n, _, err := remote.Read(buf)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					s.logger.Warn().Err(err).Str("kind", kind).Msg("error reading from screen share track")
				}
				return
			}

			_, err = local.Write(buf[:n])
			if err != nil && !errors.Is(err, io.ErrClosedPipe) {
				s.logger.Warn().Err(err).Str("kind", kind).Msg("error writing to screen share relay track")
			}
		}
	}
}

// OnSessionDisconnected should be called when a session disconnects to clean up screen share if needed.
func (s *ScreenShareManager) OnSessionDisconnected(sessionID string) {
	s.mu.RLock()
	isSharing := s.active && s.sharingSessionID == sessionID
	s.mu.RUnlock()

	if isSharing {
		s.ForceStop()
	}
}

// SendPLI sends a Picture Loss Indication to the screen share source to request a keyframe.
func (s *ScreenShareManager) SendPLI(connection *webrtc.PeerConnection) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.sourceVideoTrack == nil {
		return
	}

	err := connection.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{
			MediaSSRC: uint32(s.sourceVideoTrack.SSRC()),
		},
	})
	if err != nil {
		s.logger.Warn().Err(err).Msg("failed to send PLI for screen share")
	}
}
