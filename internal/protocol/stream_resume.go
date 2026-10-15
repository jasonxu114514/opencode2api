package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ForwardStreamResumable relays one upstream SSE stream to the client and,
// when the stream dies mid-flight after visible text was delivered, asks
// resumer for a continuation segment and splices its frames into the same
// downstream stream. With resumer == nil or maxResumes <= 0 it behaves
// exactly like the historical ForwardStream.
func ForwardStreamResumable(ctx context.Context, w http.ResponseWriter, reader io.Reader, protocol Protocol, model string, resumer StreamResumer, maxResumes int) (Usage, bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, fmt.Errorf("response writer does not support streaming")
	}
	observer := newStreamUsageObserver(protocol)
	first := reader
	resumes := 0
	for {
		_, copyErr := io.Copy(&sseFlushWriter{writer: w, flusher: flusher}, io.TeeReader(reader, observer))
		// Snapshot the torn tail before Finish clears the frame buffer.
		tornTail := observer.Pending()
		usage := observer.Finish()
		if observer.ErrorTermination() {
			reportResumeSkipped(resumer, "upstream_error_event", observer.visibleBytes, observer.reasoningBytes, observer.toolSeen)
			closeCurrentIfNotFirst(reader, first)
			if copyErr != nil {
				return usage, observer.Reported(), copyErr
			}
			return usage, observer.Reported(), errStreamUpstreamFailure
		}
		if observer.NormalTermination() && observer.ParseError() == nil {
			closeCurrentIfNotFirst(reader, first)
			return usage, observer.Reported(), copyErr
		}
		if ClientCanceled(ctx, copyErr) {
			// A client that walked away must see no synthetic error and no
			// resume attempt; the bytes would go nowhere.
			reportResumeSkipped(resumer, "client_canceled", observer.visibleBytes, observer.reasoningBytes, observer.toolSeen)
			closeCurrentIfNotFirst(reader, first)
			if copyErr == nil {
				copyErr = ctx.Err()
			}
			return usage, observer.Reported(), copyErr
		}
		canResumeText := observer.TextRunes() > 0
		canResumeReasoning := observer.TextRunes() == 0 && observer.ReasoningRunes() > 0
		// Unexpected termination — the only resume window.
		if resumer != nil && resumes < maxResumes && (canResumeText || canResumeReasoning) && !observer.ToolSeen() && !ClientCanceled(ctx, nil) {
			if resumeObserver, ok := resumer.(StreamResumeObserver); ok {
				resumeObserver.ResumeEligible(observer.visibleBytes, observer.reasoningBytes, observer.toolSeen)
			}
			tail := observer.TailRunes(200)
			if terminator := missingRecordTerminator(tornTail); len(terminator) > 0 {
				// The dead stream stopped mid-frame and those bytes are
				// already downstream; close the torn record now so neither
				// the pings nor the continuation's first frame join it and
				// get dropped by the client parser.
				if _, err := w.Write(terminator); err != nil {
					return usage, observer.Reported(), err
				}
				flusher.Flush()
			}
			next, resumeErr := resumeWithPing(ctx, w, flusher, resumer, observer.Text(), observer.Reasoning())
			if resumeErr == nil {
				closeCurrentIfNotFirst(reader, first)
				// Clear the dead segment's parse fault so a clean
				// continuation is not misclassified as failed; a systemic
				// fault re-surfaces on the new segment by itself.
				observer.parseErr = nil
				if canResumeText {
					reader = newLCPTrimReader(next, tail)
				} else {
					reader = next
				}
				resumes++
				continue
			}
			if ClientCanceled(ctx, resumeErr) {
				closeCurrentIfNotFirst(reader, first)
				return usage, observer.Reported(), resumeErr
			}
			// The resume failed; fall through and emit the in-band error
			// with the ORIGINAL cause, exactly as the no-resume path does.
		} else if resumer != nil {
			reason := "resume_limit"
			switch {
			case resumes < maxResumes && observer.ToolSeen():
				reason = "tool_seen"
			case resumes < maxResumes && observer.TextRunes() == 0 && observer.ReasoningRunes() == 0:
				reason = "no_visible_text"
			case resumes < maxResumes && ClientCanceled(ctx, nil):
				reason = "client_canceled"
			}
			reportResumeSkipped(resumer, reason, observer.visibleBytes, observer.reasoningBytes, observer.toolSeen)
		}
		cause := observer.ParseError()
		if copyErr != nil {
			cause = copyErr
		}
		if cause == nil {
			cause = errSSEUnexpectedEOF
		}
		emitter := newBridgeStreamEmitter(w, flusher, protocol, model)
		if err := emitUnexpectedStreamError(emitter, cause); err != nil {
			closeCurrentIfNotFirst(reader, first)
			return usage, observer.Reported(), err
		}
		closeCurrentIfNotFirst(reader, first)
		return usage, observer.Reported(), cause
	}
}

// closeCurrentIfNotFirst closes the active segment reader once it is done.
// The first reader is owned by the caller and is never closed here; every
// resumed segment reader owns its upstream response body and must be
// released to cancel its attempt context.
func closeCurrentIfNotFirst(reader io.Reader, first io.Reader) {
	if reader == first {
		return
	}
	if closer, ok := reader.(io.Closer); ok {
		_ = closer.Close()
	}
}

func reportResumeSkipped(resumer StreamResumer, reason string, visibleBytes, reasoningBytes int, toolSeen bool) {
	if observer, ok := resumer.(StreamResumeObserver); ok {
		observer.ResumeSkipped(reason, visibleBytes, reasoningBytes, toolSeen)
	}
}

// missingRecordTerminator returns the bytes needed to close an SSE record
// left open by a torn frame: both newlines when it ended mid-line, the
// missing one when it stopped after a single newline, nothing when the
// record boundary is already complete.
func missingRecordTerminator(tail []byte) []byte {
	if len(tail) == 0 {
		return nil
	}
	newlines := 0
	for i := len(tail) - 1; i >= 0 && (tail[i] == '\n' || tail[i] == '\r'); i-- {
		if tail[i] == '\n' {
			newlines++
		}
	}
	switch newlines {
	case 0:
		return []byte("\n\n")
	case 1:
		return []byte("\n")
	default:
		return nil
	}
}

// resumeWithPing fetches the continuation segment while writing an SSE
// comment every 10s so intermediaries do not time the connection out during
// the fetch. The goroutine is fully stopped before returning, so no ping can
// be written after the next frames start flowing.
func resumeWithPing(ctx context.Context, w io.Writer, flusher http.Flusher, resumer StreamResumer, partial string, reasoning string) (io.Reader, error) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// A ping write error means the client is gone; the caller's
				// context check decides what to do, not this ticker.
				_, _ = io.WriteString(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	}()
	reader, err := resumer.ResumeSegment(partial, reasoning)
	close(stop)
	<-done
	return reader, err
}

const lcpMaxOverlapRunes = 200

const lcpMinOverlapRunes = 4

type lcpTrimReader struct {
	src     io.Reader
	tail    string
	pending []byte
	buf     []byte
	tmp     []byte
	done    bool
	srcEOF  bool
	srcErr  error
}

// newLCPTrimReader returns a frame-aware reader over a continuation segment.
// Frames pass through unchanged until the first frame whose JSON carries a
// non-empty choices[0].delta.content; that frame has its longest overlap with
// the delivered tail trimmed. Every parse/rewrite failure passes the frame
// through unchanged (fail-open): dedup must never break the stream.
func newLCPTrimReader(src io.Reader, tail string) io.Reader {
	return &lcpTrimReader{src: src, tail: tail, tmp: make([]byte, 32<<10)}
}

func (reader *lcpTrimReader) Read(p []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.done {
			// After the scan ends, buffered remainder is stream bytes that
			// must still reach the client verbatim before passthrough.
			if len(reader.buf) > 0 {
				reader.pending = reader.buf
				reader.buf = nil
				continue
			}
			if reader.srcEOF {
				return 0, reader.srcErr
			}
			return reader.src.Read(p)
		}
		if index, width := nextSSEBoundary(reader.buf); index >= 0 {
			frame := make([]byte, index+width)
			copy(frame, reader.buf[:index+width])
			reader.buf = reader.buf[index+width:]
			trimmed, done := reader.transform(frame)
			reader.pending = trimmed
			reader.done = done
			continue
		}
		if reader.srcEOF {
			// Residual bytes without a final boundary are served as-is.
			reader.pending = reader.buf
			reader.buf = nil
			reader.done = true
			continue
		}
		n, err := reader.src.Read(reader.tmp)
		if n > 0 {
			reader.buf = append(reader.buf, reader.tmp[:n]...)
		}
		if err != nil {
			reader.srcEOF = true
			if err != io.EOF {
				reader.srcErr = err
			} else {
				reader.srcErr = io.EOF
			}
		}
	}
	n := copy(p, reader.pending)
	reader.pending = reader.pending[n:]
	return n, nil
}

// Close releases the underlying segment reader, cancelling its attempt
// context through the gateway's attemptBody wrapper.
func (reader *lcpTrimReader) Close() error {
	if closer, ok := reader.src.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// transform inspects one complete SSE frame and decides whether it is the
// first text-carrying frame. The second return value reports whether the
// scan is over and every later byte must pass through untouched.
func (reader *lcpTrimReader) transform(frame []byte) ([]byte, bool) {
	data, ok := sseFrameData(frame)
	if !ok {
		// A comment or event-only frame carries no JSON to inspect; keep
		// scanning.
		return frame, false
	}
	if data == "[DONE]" {
		return frame, true
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		// Unparseable data: fail open and stop scanning.
		return frame, true
	}
	delta, text, ok := firstChatDelta(payload)
	if !ok {
		return frame, false
	}
	k := longestOverlap(text, reader.tail, lcpMaxOverlapRunes)
	if k == 0 {
		return frame, true
	}
	// k counts runes; slicing bytes would split multi-byte characters.
	delta["content"] = string([]rune(text)[k:])
	rewritten, err := json.Marshal(payload)
	if err != nil {
		// Rewrite failure: fail open with the original frame.
		return frame, true
	}
	return []byte("data: " + string(rewritten) + "\n\n"), true
}

// sseFrameData joins a frame's data: lines via readSSE rules; false means no
// data payload (comments, event-only frames).
func sseFrameData(frame []byte) (string, bool) {
	var joined string
	found := false
	_ = readSSE(bytes.NewReader(frame), func(eventName, value string) error {
		if found {
			joined += "\n" + value
		} else {
			joined = value
			found = true
		}
		return nil
	})
	return joined, found
}

func firstChatDelta(payload map[string]any) (map[string]any, string, bool) {
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, "", false
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil, "", false
	}
	delta, ok := choice["delta"].(map[string]any)
	if !ok {
		return nil, "", false
	}
	content, ok := delta["content"].(string)
	if !ok || content == "" {
		return nil, "", false
	}
	return delta, content, true
}

// longestOverlap returns the largest k in [minOverlap, min(runeLen(text),
// runeLen(tail), maxK)] whose text prefix equals the tail suffix. Comparison
// is rune-based so multi-byte characters are never split.
func longestOverlap(text, tail string, maxK int) int {
	prefix := []rune(text)
	if len(prefix) > maxK {
		prefix = prefix[:maxK]
	}
	suffix := []rune(tail)
	if len(suffix) > maxK {
		suffix = suffix[len(suffix)-maxK:]
	}
	limit := min(len(prefix), len(suffix))
	for k := limit; k >= lcpMinOverlapRunes; k-- {
		if string(prefix[:k]) == string(suffix[len(suffix)-k:]) {
			return k
		}
	}
	return 0
}
