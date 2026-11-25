package broadcast

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cliamp-server/library"
	"cliamp-server/mp3frame"
)

// testFrame builds one MPEG-1 Layer III frame (no CRC) filled with fill.
// brIdx/srIdx are header table indexes: 9/0 is 128 kbps at 44.1 kHz.
func testFrame(t *testing.T, brIdx, srIdx byte, mono bool, fill byte) []byte {
	t.Helper()
	mode := byte(1) // joint stereo
	if mono {
		mode = 3
	}
	h := [4]byte{0xFF, 0xFB, brIdx<<4 | srIdx<<2, mode << 6}
	f, err := mp3frame.ParseHeader(h)
	if err != nil {
		t.Fatalf("build test frame: %v", err)
	}
	data := bytes.Repeat([]byte{fill}, f.FrameSize)
	copy(data, h[:])
	return data
}

// testInfoFrame builds a LAME-style "Info" header frame in the station profile.
func testInfoFrame(t *testing.T) []byte {
	t.Helper()
	data := testFrame(t, 9, 0, false, 0)
	copy(data[4+32:], "Info")
	return data
}

func writeFile(t *testing.T, dir, name string, parts ...[]byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, bytes.Join(parts, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// id3v2 returns a minimal ID3v2.3 tag with size bytes of zero padding.
func id3v2(size int) []byte {
	tag := []byte{'I', 'D', '3', 3, 0, 0,
		byte(size >> 21 & 0x7F), byte(size >> 14 & 0x7F), byte(size >> 7 & 0x7F), byte(size & 0x7F)}
	return append(tag, make([]byte, size)...)
}

type readChunk struct {
	data  []byte
	track TrackInfo
}

// readRing reads at least n bytes from the start of the ring, keeping the
// track reported for each chunk.
func readRing(t *testing.T, hub *Hub, n int) []readChunk {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var chunks []readChunk
	total := 0
	pos := int64(0)
	buf := make([]byte, 8192)
	for total < n {
		got, next, track, err := hub.Ring().Read(ctx, pos, buf)
		if err != nil {
			t.Fatalf("read ring after %d bytes: %v", total, err)
		}
		chunks = append(chunks, readChunk{data: bytes.Clone(buf[:got]), track: track})
		total += got
		pos = next
	}
	return chunks
}

func runHub(t *testing.T, tracks []library.Track) *Hub {
	t.Helper()
	hub := NewHub("test", &cyclicTrackSource{tracks: tracks}, 64, 0)
	hub.SetPassthrough(true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		hub.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("hub did not stop after cancellation")
		}
	})
	return hub
}

func TestPassthroughCopiesFramesBackToBack(t *testing.T) {
	dir := t.TempDir()
	a := [][]byte{testFrame(t, 9, 0, false, 0xA1), testFrame(t, 9, 0, false, 0xA2), testFrame(t, 9, 0, false, 0xA3)}
	b := [][]byte{testFrame(t, 9, 0, true, 0xB1), testFrame(t, 9, 0, true, 0xB2)}

	// Track A has an ID3v2 tag, a LAME Info frame and a trailing ID3v1 tag,
	// none of which may reach listeners.
	id3v1 := append([]byte("TAG"), make([]byte, 125)...)
	pathA := writeFile(t, dir, "a.mp3", id3v2(64), testInfoFrame(t), a[0], a[1], a[2], id3v1)
	pathB := writeFile(t, dir, "b.mp3", b[0], b[1])

	hub := runHub(t, []library.Track{
		{Path: pathA, Title: "A", Artist: "Artist A"},
		{Path: pathB, Title: "B"},
	})

	want := bytes.Join(append(append([][]byte{}, a...), b...), nil)
	chunks := readRing(t, hub, len(want))

	var got []byte
	for _, c := range chunks {
		wantTitle := "A"
		if len(got) >= len(a)*len(a[0]) {
			wantTitle = "B"
		}
		if c.track.Title != wantTitle {
			t.Fatalf("bytes at offset %d reported track %q, want %q", len(got), c.track.Title, wantTitle)
		}
		got = append(got, c.data...)
	}
	if !bytes.Equal(got[:len(want)], want) {
		t.Fatal("ring content differs from the source frames (tags or Info frame leaked, or frames reordered)")
	}
	if cur := hub.CurrentTrack(); cur.Title != "A" && cur.Title != "B" {
		t.Fatalf("current track = %+v", cur)
	}
}

func TestPassthroughSkipsTracksOutsideStationProfile(t *testing.T) {
	dir := t.TempDir()
	bad := writeFile(t, dir, "192k.mp3", testFrame(t, 11, 0, false, 0xEE), testFrame(t, 11, 0, false, 0xEE)) // 192 kbps
	good := []byte(nil)
	for range 3 {
		good = append(good, testFrame(t, 9, 0, false, 0x55)...)
	}
	okPath := writeFile(t, dir, "ok.mp3", good)

	hub := runHub(t, []library.Track{{Path: bad, Title: "bad"}, {Path: okPath, Title: "ok"}})

	chunks := readRing(t, hub, len(good))
	if chunks[0].track.Title != "ok" {
		t.Fatalf("first audio reported track %q, want %q", chunks[0].track.Title, "ok")
	}
	if !bytes.Equal(chunks[0].data, good[:len(chunks[0].data)]) {
		t.Fatal("frames from the out-of-profile track reached the ring")
	}
}

func TestPassthroughPacesAtRealTime(t *testing.T) {
	dir := t.TempDir()
	var frames []byte
	for range 40 { // 40 × 1152 samples ≈ 1.045 s at 44.1 kHz
		frames = append(frames, testFrame(t, 9, 0, false, 0x11)...)
	}
	path := writeFile(t, dir, "t.mp3", frames)

	start := time.Now()
	hub := runHub(t, []library.Track{{Path: path, Title: "t"}})
	readRing(t, hub, len(frames))
	elapsed := time.Since(start)

	// The last frame is released once the 39 before it have "played".
	if min := 39 * 1152 * time.Second / 44100; elapsed < min-50*time.Millisecond {
		t.Fatalf("40 frames published in %v, faster than real time (%v)", elapsed, min)
	}
}

func TestIsInfoFrame(t *testing.T) {
	if !isInfoFrame(mustParse(t, testInfoFrame(t))) {
		t.Error("stereo Info frame not detected")
	}
	mono := testFrame(t, 9, 0, true, 0)
	copy(mono[4+17:], "Xing")
	if !isInfoFrame(mustParse(t, mono)) {
		t.Error("mono Xing frame not detected")
	}
	if isInfoFrame(mustParse(t, testFrame(t, 9, 0, false, 0x49))) {
		t.Error("audio frame detected as Info frame")
	}
}

func mustParse(t *testing.T, data []byte) mp3frame.Frame {
	t.Helper()
	f, err := mp3frame.ParseHeader([4]byte(data[:4]))
	if err != nil {
		t.Fatal(err)
	}
	f.Data = data
	return f
}
