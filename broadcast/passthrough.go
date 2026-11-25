package broadcast

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"cliamp-server/library"
	"cliamp-server/mp3frame"
	"cliamp-server/transcode"
)

// errTrackProfile reports a file whose MP3 frames do not match the station
// profile, so it cannot be copied to listeners without re-encoding.
var errTrackProfile = errors.New("track is not in the station MP3 profile")

// SetPassthrough makes the hub copy MP3 frames straight from the source files
// instead of decoding and re-encoding them. Files must already be MPEG-1
// Layer III at the station bitrate and sample rate; any other file is skipped.
// Call it before Run.
func (h *Hub) SetPassthrough(enabled bool) {
	h.passthrough = enabled
}

// runPassthrough plays tracks back to back by copying their frames to the ring
// at real-time pace. No FFmpeg process is involved.
func (h *Hub) runPassthrough(ctx context.Context) {
	p := newPacer()
	defer p.stop()

	for ctx.Err() == nil {
		track := h.source.Next()
		published, err := h.playFrames(ctx, p, track)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("cannot play track in passthrough mode", "station", h.stationID, "path", track.Path, "error", err)
		}
		// A track that produced nothing (unreadable, empty, wrong profile)
		// must not turn the loop into a busy spin over a broken library.
		if published == 0 && !waitFor(ctx, decoderRetryDelay) {
			return
		}
	}
}

// playFrames copies one file's frames to the ring and returns how many were
// published. It stops at the first frame outside the station profile.
func (h *Hub) playFrames(ctx context.Context, p *pacer, track library.Track) (int, error) {
	f, err := os.Open(track.Path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	reader, err := mp3frame.NewReader(f)
	if err != nil {
		return 0, err
	}

	published := 0
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				err = nil
			}
			return published, err
		}
		if published == 0 && isInfoFrame(frame) {
			continue
		}
		if !inStationProfile(frame) {
			return published, profileError(frame)
		}

		var transition *TrackInfo
		if published == 0 {
			info := TrackInfo{Title: track.Title, Artist: track.Artist, Album: track.Album}
			h.currentTrack.Store(info)
			transition = &info
			slog.Info("now playing", "title", track.Title, "artist", track.Artist, "path", track.Path)
		}
		h.ring.WriteFrame(frame.Data, transition)
		published++

		if !p.wait(ctx, frame) {
			return published, nil
		}
	}
}

func inStationProfile(f mp3frame.Frame) bool {
	return f.Version == 1 && f.Layer == 3 &&
		f.Bitrate == transcode.OutputBitrate && f.SampleRate == transcode.OutputSampleRate
}

// isInfoFrame reports whether f is the Xing/Info header frame encoders put at
// the start of a file. It carries no audio meant for playback.
func isInfoFrame(f mp3frame.Frame) bool {
	return infoTag(f) != ""
}

// infoTag returns "Xing" (LAME: VBR), "Info" (LAME: CBR) or "" when f is an
// audio frame. The tag follows the MPEG-1 side information: 17 bytes in mono
// (channel mode bits 7-6 of the fourth header byte == 3), 32 otherwise.
func infoTag(f mp3frame.Frame) string {
	off := 4 + 32
	if f.Header[3]>>6 == 3 {
		off = 4 + 17
	}
	if off+4 > len(f.Data) {
		return ""
	}
	switch tag := string(f.Data[off : off+4]); tag {
	case "Xing", "Info":
		return tag
	}
	return ""
}

// passthroughCheckFrames is how many leading audio frames CheckPassthrough
// inspects: enough to catch VBR files that lack a Xing header.
const passthroughCheckFrames = 32

// CheckPassthrough reports whether a passthrough station can stream path: a
// CBR file (no Xing header) whose leading frames are in the station profile.
// It reads a few kilobytes, so a whole library can be checked at startup.
func CheckPassthrough(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	reader, err := mp3frame.NewReader(f)
	if err != nil {
		return err
	}
	checked := 0
	for checked < passthroughCheckFrames {
		frame, err := reader.ReadFrame()
		if err != nil {
			if checked > 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
				return nil // short file, every frame was fine
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return errors.New("no MP3 audio frames")
			}
			return err
		}
		if checked == 0 {
			switch infoTag(frame) {
			case "Xing":
				return fmt.Errorf("%w: variable bitrate (Xing header)", errTrackProfile)
			case "Info":
				continue
			}
		}
		if !inStationProfile(frame) {
			return profileError(frame)
		}
		checked++
	}
	return nil
}

func profileError(frame mp3frame.Frame) error {
	return fmt.Errorf("%w: MPEG-%d layer %d, %d kbps, %d Hz (want MPEG-1 layer 3, %d kbps, %d Hz)",
		errTrackProfile, frame.Version, frame.Layer, frame.Bitrate, frame.SampleRate,
		transcode.OutputBitrate, transcode.OutputSampleRate)
}

// pacer releases frames at the rate their samples play back, measured from a
// fixed epoch so that per-frame timer jitter does not accumulate.
type pacer struct {
	epoch   time.Time
	samples int64
	timer   *time.Timer
}

func newPacer() *pacer {
	t := time.NewTimer(0)
	if !t.Stop() {
		<-t.C
	}
	return &pacer{epoch: time.Now(), timer: t}
}

func (p *pacer) stop() { p.timer.Stop() }

// wait accounts for frame and sleeps until it is due. It returns false if ctx
// was cancelled while waiting.
func (p *pacer) wait(ctx context.Context, frame mp3frame.Frame) bool {
	p.samples += int64(frame.Samples)
	rate := int64(frame.SampleRate)
	elapsed := time.Duration(p.samples/rate)*time.Second +
		time.Duration(p.samples%rate)*time.Second/time.Duration(rate)
	wait := time.Until(p.epoch.Add(elapsed))
	if wait <= 0 {
		return true
	}
	p.timer.Reset(wait)
	select {
	case <-p.timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
