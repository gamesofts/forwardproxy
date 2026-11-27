package forwardproxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// smallChunkReader simulates HTTP/TLS data arriving in multiple reads.
type smallChunkReader struct {
	io.Reader
	max int
}

func (r smallChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		p = p[:r.max]
	}
	return r.Reader.Read(p)
}

func appendPaddingFrame(dst []byte, payload []byte, padding []byte) []byte {
	var header [3]byte
	binary.BigEndian.PutUint16(header[:2], uint16(len(payload)))
	header[2] = byte(len(padding))
	dst = append(dst, header[:]...)
	dst = append(dst, payload...)
	return append(dst, padding...)
}

// Verify the first payload bytes reach the target before the rest of the
// frame (and padding) arrive. A frame-wide io.ReadFull would deadlock here.
func TestRemovePaddingForwardsPartialFrameImmediately(t *testing.T) {
	source, inbound := net.Pipe()
	target, outbound := net.Pipe()
	defer source.Close()
	defer inbound.Close()
	defer target.Close()
	defer outbound.Close()

	result := make(chan error, 1)
	go func() {
		_, err := flushingIoCopy(target, source, make([]byte, 64*1024), RemovePadding)
		result <- err
	}()

	payload := []byte("hello-world")
	first := appendPaddingFrame(nil, payload[:5], nil)
	// Announce the full payload and two padding bytes, but send only the
	// header and first 5 payload bytes. The rest comes after receipt.
	binary.BigEndian.PutUint16(first[:2], uint16(len(payload)))
	first[2] = 2

	writeResult := make(chan error, 1)
	go func() {
		_, err := inbound.Write(first)
		writeResult <- err
	}()
	if err := outbound.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	early := make([]byte, 5)
	if _, err := io.ReadFull(outbound, early); err != nil {
		t.Fatalf("no early payload was forwarded: %v", err)
	}
	if string(early) != "hello" {
		t.Fatalf("unexpected early payload %q", early)
	}
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}

	go func() {
		_, err := inbound.Write(append([]byte("-world"), 0xab, 0xcd))
		writeResult <- err
	}()
	later := make([]byte, 6)
	if _, err := io.ReadFull(outbound, later); err != nil {
		t.Fatalf("remaining payload not forwarded: %v", err)
	}
	if string(later) != "-world" {
		t.Fatalf("unexpected remaining payload %q", later)
	}
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}
	_ = inbound.Close()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("frame completed with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unpadding goroutine did not exit")
	}
}

// Eight padded frames, including zero-length and >32KiB payloads, must
// decode exactly. The ninth frame is raw data, not a padding frame.
func TestRemovePaddingFragmentationAndRawTransition(t *testing.T) {
	encoded := make([]byte, 0, 70*1024)
	want := make([]byte, 0, 70*1024)
	for i := 0; i < NumFirstPaddings; i++ {
		var payload []byte
		switch i {
		case 0:
			payload = []byte("first")
		case 1:
			payload = nil
		case 2:
			payload = bytes.Repeat([]byte{0x71}, 60000)
		default:
			payload = bytes.Repeat([]byte{byte(i)}, i*13)
		}
		pad := bytes.Repeat([]byte{byte(0xe0 + i)}, i*31)
		encoded = appendPaddingFrame(encoded, payload, pad)
		want = append(want, payload...)
	}
	raw := []byte{0x00, 0xfe, 0xff, 0x01, 0x02, 'r', 'a', 'w'}
	encoded = append(encoded, raw...)
	want = append(want, raw...)

	var got bytes.Buffer
	n, err := flushingIoCopy(&got, smallChunkReader{Reader: bytes.NewReader(encoded), max: 7}, make([]byte, 64*1024), RemovePadding)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) || n != int64(len(want)) {
		t.Fatalf("decoded stream differs: got %d bytes, want %d, written %d", got.Len(), len(want), n)
	}
}

func TestRemovePaddingRejectsTruncatedFrames(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  []byte
	}{
		{"partial header", []byte{0x00, 0x04}, nil},
		{"partial payload", []byte{0x00, 0x04, 0x00, 'a', 'b'}, []byte("ab")},
		{"missing padding", []byte{0x00, 0x02, 0x03, 'a', 'b', 0x07}, []byte("ab")},
		{"padding only partial", []byte{0x00, 0x00, 0x02, 0xff}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got bytes.Buffer
			n, err := flushingIoCopy(&got, bytes.NewReader(tt.input), make([]byte, 64*1024), RemovePadding)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("expected truncated frame error, got %v", err)
			}
			if !bytes.Equal(got.Bytes(), tt.want) || n != int64(len(tt.want)) {
				t.Fatalf("wrong bytes forwarded: %q, written=%d; want %q", got.Bytes(), n, tt.want)
			}
		})
	}
}

func TestRemovePaddingCleanEOFAndShortWrite(t *testing.T) {
	for _, stream := range [][]byte{nil, appendPaddingFrame(nil, []byte("ok"), nil)} {
		var dst bytes.Buffer
		n, err := flushingIoCopy(&dst, bytes.NewReader(stream), make([]byte, 64*1024), RemovePadding)
		if err != nil || n != int64(dst.Len()) {
			t.Fatalf("unexpected EOF handling: n=%d, err=%v", n, err)
		}
	}
	short := writerFunc(func(p []byte) (int, error) { return len(p) - 1, nil })
	n, err := flushingIoCopy(short, bytes.NewReader(appendPaddingFrame(nil, []byte("abc"), nil)), make([]byte, 64*1024), RemovePadding)
	if !errors.Is(err, io.ErrShortWrite) || n != 2 {
		t.Fatalf("short write handling: n=%d err=%v", n, err)
	}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

// chunkedCountingReader delivers pre-split chunks one per Read call and tracks
// how many Read calls were needed (analogous to reading HTTP/2 DATA frames).
type chunkedCountingReader struct {
	chunks [][]byte
	reads  int
}

func (r *chunkedCountingReader) Read(p []byte) (int, error) {
	r.reads++
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	if n == len(r.chunks[0]) {
		r.chunks = r.chunks[1:]
	} else {
		r.chunks[0] = r.chunks[0][n:]
	}
	return n, nil
}

// Verify each complete padded frame takes a single src.Read call instead of
// three separate reads (header, payload, padding).
func TestRemovePaddingMinimizesReadCalls(t *testing.T) {
	chunks := make([][]byte, 0, NumFirstPaddings+1)
	var want bytes.Buffer
	for i := 0; i < NumFirstPaddings; i++ {
		payload := bytes.Repeat([]byte{byte('a' + i)}, 128+i*17)
		pad := bytes.Repeat([]byte{byte(0xf0 + i)}, 13+i*5)
		chunks = append(chunks, appendPaddingFrame(nil, payload, pad))
		want.Write(payload)
	}
	raw := []byte("post-padding-raw-data")
	chunks = append(chunks, raw)
	want.Write(raw)

	src := &chunkedCountingReader{chunks: chunks}
	var dst bytes.Buffer
	n, err := flushingIoCopy(&dst, src, make([]byte, 64*1024), RemovePadding)
	if err != nil {
		t.Fatalf("flushingIoCopy failed: %v", err)
	}
	if !bytes.Equal(dst.Bytes(), want.Bytes()) || n != int64(want.Len()) {
		t.Fatalf("decoded stream mismatch: got %d bytes, want %d, written %d", dst.Len(), want.Len(), n)
	}
	// 8 padded frame reads + 1 raw chunk read + 1 EOF read = 10 reads total
	// (previous 3-read-per-frame implementation required 26 reads).
	if wantReads := NumFirstPaddings + 2; src.reads != wantReads {
		t.Fatalf("src.Read called %d times; want %d", src.reads, wantReads)
	}
}

// Verify multiple padded frames and trailing raw bytes arriving in a single
// Read call are decoded cleanly, both when followed by more raw data and on EOF.
func TestRemovePaddingCoalescedFramesAndTrailingRawInSingleRead(t *testing.T) {
	var firstChunk []byte
	var want bytes.Buffer
	for i := 0; i < NumFirstPaddings; i++ {
		var payload []byte
		if i%2 == 0 {
			payload = bytes.Repeat([]byte{byte('0' + i)}, 32+i)
		}
		pad := bytes.Repeat([]byte{0xee}, i*3)
		firstChunk = appendPaddingFrame(firstChunk, payload, pad)
		want.Write(payload)
	}
	trailingRaw := []byte("trailing-raw-in-same-read")
	firstChunk = append(firstChunk, trailingRaw...)
	want.Write(trailingRaw)

	t.Run("single read with immediate EOF", func(t *testing.T) {
		var dst bytes.Buffer
		n, err := flushingIoCopy(&dst, bytes.NewReader(firstChunk), make([]byte, 64*1024), RemovePadding)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(dst.Bytes(), want.Bytes()) || n != int64(want.Len()) {
			t.Fatalf("mismatch: got %d, want %d, written %d", dst.Len(), want.Len(), n)
		}
	})

	t.Run("single read followed by more raw chunks", func(t *testing.T) {
		moreRaw := []byte("-and-next-raw-chunk")
		src := &chunkedCountingReader{chunks: [][]byte{firstChunk, moreRaw}}
		var dst bytes.Buffer
		n, err := flushingIoCopy(&dst, src, make([]byte, 64*1024), RemovePadding)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expected := append(append([]byte(nil), want.Bytes()...), moreRaw...)
		if !bytes.Equal(dst.Bytes(), expected) || n != int64(len(expected)) {
			t.Fatalf("mismatch: got %d, want %d, written %d", dst.Len(), len(expected), n)
		}
		if src.reads != 3 {
			t.Fatalf("src.Read called %d times; want 3", src.reads)
		}
	})
}

func BenchmarkRemovePaddingFirst8Frames(b *testing.B) {
	frames := make([][]byte, NumFirstPaddings)
	var totalPayload int64
	for i := 0; i < NumFirstPaddings; i++ {
		payload := bytes.Repeat([]byte{byte(i + 1)}, 1024)
		pad := make([]byte, 64)
		frames[i] = appendPaddingFrame(nil, payload, pad)
		totalPayload += int64(len(payload))
	}
	buf := make([]byte, 64*1024)
	src := &chunkedCountingReader{}
	b.ReportAllocs()
	b.SetBytes(totalPayload)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		src.chunks = frames
		src.reads = 0
		if _, err := flushingIoCopy(io.Discard, src, buf, RemovePadding); err != nil {
			b.Fatal(err)
		}
	}
}
