package upstream

import (
	"bufio"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// endlessReader 永远返回同一个字节、永不 EOF：模拟"上游不断开也不发换行"。
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// TestReadBoundedLineRejectsOversizedFrame is the core DoS fix: ReadString grew
// the returned string without limit, so a peer that never sent '\n' could
// exhaust the process heap.
func TestReadBoundedLineRejectsOversizedFrame(t *testing.T) {
	const max = 4096
	br := bufio.NewReader(endlessReader{})

	_, err := readBoundedLine(br, max)
	if !errors.Is(err, errSSELineTooLong) {
		t.Fatalf("err = %v, want errSSELineTooLong (unbounded read regression)", err)
	}
}

// TestReadBoundedLineStillReturnsNormalLines guards the happy path.
func TestReadBoundedLineStillReturnsNormalLines(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("data: {\"a\":1}\n\nhello\n"))
	for _, want := range []string{"data: {\"a\":1}\n", "\n", "hello\n"} {
		got, err := readBoundedLine(br, 1<<20)
		if err != nil && err != io.EOF {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}

// TestReadBoundedLineSpansBufferBoundaries: a line longer than bufio's internal
// buffer must still be assembled correctly when it is within the cap.
func TestReadBoundedLineSpansBufferBoundaries(t *testing.T) {
	body := strings.Repeat("x", 9000) + "\n"
	got, err := readBoundedLine(bufio.NewReader(strings.NewReader(body)), 1<<20)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != body {
		t.Fatalf("long-but-legal line was not reassembled: len=%d want=%d", len(got), len(body))
	}
}

// TestAggregateRejectsOversizedFrame runs the fix through the real entry point.
func TestAggregateRejectsOversizedFrame(t *testing.T) {
	_, err := Aggregate(endlessReader{})
	if err == nil {
		t.Fatal("Aggregate accepted an endless unterminated frame")
	}
	if !errors.Is(err, errSSELineTooLong) {
		t.Fatalf("err = %v, want errSSELineTooLong", err)
	}
}

// TestAggregateBoundsAccumulatedContent: many individually-legal frames must not
// add up to unbounded memory.
func TestAggregateBoundsAccumulatedContent(t *testing.T) {
	// ~64 KiB of content per frame, repeated past the 8 MiB ceiling.
	chunk := `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"` +
		strings.Repeat("y", 64<<10) + `"}}]}` + "\n\n"
	stream := strings.Repeat(chunk, (maxAccumulatedBytes/(64<<10))+4)

	_, err := Aggregate(strings.NewReader(stream))
	if err == nil {
		t.Fatal("Aggregate accepted content beyond the accumulator ceiling")
	}
	if !errors.Is(err, errAccumulatedTooLarge) {
		t.Fatalf("err = %v, want errAccumulatedTooLarge", err)
	}
}

// TestStreamRejectsOversizedFrame checks the passthrough path too.
func TestStreamRejectsOversizedFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, endlessReader{})
	if err == nil {
		t.Fatal("stream accepted an endless unterminated frame")
	}
	if !errors.Is(err, errSSELineTooLong) {
		t.Fatalf("err = %v, want errSSELineTooLong", err)
	}
}

// TestStreamStillForwardsPartialLineOnReadError is the regression for an
// over-eager early return: when the upstream dies mid-line, the bytes already
// received must still reach the client, and an error frame must still be
// emitted before [DONE].
func TestStreamStillForwardsPartialLineOnReadError(t *testing.T) {
	// Emits one full frame, then a partial line, then fails.
	r := io.MultiReader(
		strings.NewReader("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"),
		&failingReader{data: []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"trunc"), err: io.ErrUnexpectedEOF},
	)
	rec := httptest.NewRecorder()
	_ = Stream(rec, r)

	body := rec.Body.String()
	if !strings.Contains(body, "partial") {
		t.Errorf("completed frame was dropped:\n%s", body)
	}
	if !strings.Contains(body, "upstream_stream_error") {
		t.Errorf("read failure did not produce an error frame:\n%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("stream did not terminate with [DONE]:\n%s", body)
	}
}

// failingReader returns its payload once, then the configured error.
type failingReader struct {
	data []byte
	err  error
	done bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.done {
		return 0, f.err
	}
	f.done = true
	n := copy(p, f.data)
	return n, nil
}

// TestStreamBoundsAccumulatedContent mirrors the aggregate case for the
// normalized stream. The client must be told the response was too large rather
// than seeing content stop and a [DONE] that looks like a clean finish.
func TestStreamBoundsAccumulatedContent(t *testing.T) {
	chunk := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"` +
		strings.Repeat("z", 64<<10) + `"}}]}` + "\n\n"
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(strings.Repeat(chunk, (maxAccumulatedBytes/(64<<10))+4)))

	body := rec.Body.String()
	if !strings.Contains(body, "response_too_large") {
		t.Errorf("client was not told the response was too large:\n%.300s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("stream did not terminate with [DONE]:\n%.200s", body)
	}
	if err == nil {
		t.Error("Stream must return a non-nil error for the caller to log")
	}
	if strings.Contains(err.Error(), "contained no content") {
		t.Errorf("size overflow misreported as empty content: %v", err)
	}
}

// TestStreamNormalContentUnaffected: the cap must not disturb normal streams.
func TestStreamNormalContentUnaffected(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(sseSample)); err != nil {
		t.Fatalf("normal stream failed: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Errorf("normal content missing:\n%s", rec.Body.String())
	}
}

const sseSample = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

